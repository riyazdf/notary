package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/notary/certs"
	"github.com/docker/notary/client/changelist"
	"github.com/docker/notary/cryptoservice"
	"github.com/docker/notary/trustmanager"
	"github.com/docker/notary/tuf"
	tufclient "github.com/docker/notary/tuf/client"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/tuf/keys"
	"github.com/docker/notary/tuf/signed"
	"github.com/docker/notary/tuf/store"
)

const (
	maxSize = 5 << 20
)

func init() {
	data.SetDefaultExpiryTimes(
		map[string]int{
			"root":     3650,
			"targets":  1095,
			"snapshot": 1095,
		},
	)
}

// ErrRepoNotInitialized is returned when trying to can publish on an uninitialized
// notary repository
type ErrRepoNotInitialized struct{}

// ErrRepoNotInitialized is returned when trying to can publish on an uninitialized
// notary repository
func (err *ErrRepoNotInitialized) Error() string {
	return "Repository has not been initialized"
}

// ErrExpired is returned when the metadata for a role has expired
type ErrExpired struct {
	signed.ErrExpired
}

// ErrInvalidRemoteRole is returned when the server is requested to manage
// an unsupported key type
type ErrInvalidRemoteRole struct {
	Role string
}

func (e ErrInvalidRemoteRole) Error() string {
	return fmt.Sprintf(
		"notary does not support the server managing the %s key", e.Role)
}

const (
	tufDir = "tuf"
)

// ErrRepositoryNotExist gets returned when trying to make an action over a repository
/// that doesn't exist.
var ErrRepositoryNotExist = errors.New("repository does not exist")

// NotaryRepository stores all the information needed to operate on a notary
// repository.
type NotaryRepository struct {
	baseDir       string
	gun           string
	baseURL       string
	tufRepoPath   string
	fileStore     store.MetadataStore
	CryptoService signed.CryptoService
	tufRepo       *tuf.Repo
	roundTrip     http.RoundTripper
	CertManager   *certs.Manager
}

// repositoryFromKeystores is a helper function for NewNotaryRepository that
// takes some basic NotaryRepository parameters as well as keystores (in order
// of usage preference), and returns a NotaryRepository.
func repositoryFromKeystores(baseDir, gun, baseURL string, rt http.RoundTripper,
	keyStores []trustmanager.KeyStore) (*NotaryRepository, error) {

	certManager, err := certs.NewManager(baseDir)
	if err != nil {
		return nil, err
	}

	cryptoService := cryptoservice.NewCryptoService(gun, keyStores...)

	nRepo := &NotaryRepository{
		gun:           gun,
		baseDir:       baseDir,
		baseURL:       baseURL,
		tufRepoPath:   filepath.Join(baseDir, tufDir, filepath.FromSlash(gun)),
		CryptoService: cryptoService,
		roundTrip:     rt,
		CertManager:   certManager,
	}

	fileStore, err := store.NewFilesystemStore(
		nRepo.tufRepoPath,
		"metadata",
		"json",
		"",
	)
	if err != nil {
		return nil, err
	}
	nRepo.fileStore = fileStore

	return nRepo, nil
}

// Target represents a simplified version of the data TUF operates on, so external
// applications don't have to depend on tuf data types.
type Target struct {
	Name   string
	Hashes data.Hashes
	Length int64
}

// NewTarget is a helper method that returns a Target
func NewTarget(targetName string, targetPath string) (*Target, error) {
	b, err := ioutil.ReadFile(targetPath)
	if err != nil {
		return nil, err
	}

	meta, err := data.NewFileMeta(bytes.NewBuffer(b))
	if err != nil {
		return nil, err
	}

	return &Target{Name: targetName, Hashes: meta.Hashes, Length: meta.Length}, nil
}

// Initialize creates a new repository by using rootKey as the root Key for the
// TUF repository.
func (r *NotaryRepository) Initialize(rootKeyID string, serverManagedRoles ...string) error {
	privKey, _, err := r.CryptoService.GetPrivateKey(rootKeyID)
	if err != nil {
		return err
	}

	// currently we only support server managing timestamps and snapshots, and
	// nothing else - timestamps are always managed by the server, and implicit
	// (do not have to be passed in as part of `serverManagedRoles`, so that
	// the API of Initialize doens't change).
	var serverManagesSnapshot bool
	locallyManagedKeys := []string{
		data.CanonicalTargetsRole,
		data.CanonicalSnapshotRole,
		// root is also locally managed, but that should have been created
		// already
	}
	remotelyManagedKeys := []string{data.CanonicalTimestampRole}
	for _, role := range serverManagedRoles {
		switch role {
		case data.CanonicalTimestampRole:
			continue // timestamp is already in the right place
		case data.CanonicalSnapshotRole:
			// because we put Snapshot last
			locallyManagedKeys = []string{data.CanonicalTargetsRole}
			remotelyManagedKeys = append(
				remotelyManagedKeys, data.CanonicalSnapshotRole)
			serverManagesSnapshot = true
		default:
			return ErrInvalidRemoteRole{Role: role}
		}
	}

	// Hard-coded policy: the generated certificate expires in 10 years.
	startTime := time.Now()
	rootCert, err := cryptoservice.GenerateCertificate(
		privKey, r.gun, startTime, startTime.AddDate(10, 0, 0))

	if err != nil {
		return err
	}
	r.CertManager.AddTrustedCert(rootCert)

	// The root key gets stored in the TUF metadata X509 encoded, linking
	// the tuf root.json to our X509 PKI.
	// If the key is RSA, we store it as type RSAx509, if it is ECDSA we store it
	// as ECDSAx509 to allow the gotuf verifiers to correctly decode the
	// key on verification of signatures.
	var rootKey data.PublicKey
	switch privKey.Algorithm() {
	case data.RSAKey:
		rootKey = data.NewRSAx509PublicKey(trustmanager.CertToPEM(rootCert))
	case data.ECDSAKey:
		rootKey = data.NewECDSAx509PublicKey(trustmanager.CertToPEM(rootCert))
	default:
		return fmt.Errorf("invalid format for root key: %s", privKey.Algorithm())
	}

	kdb := keys.NewDB()
	err = addKeyForRole(kdb, data.CanonicalRootRole, rootKey)
	if err != nil {
		return err
	}

	// we want to create all the local keys first so we don't have to
	// make unnecessary network calls
	for _, role := range locallyManagedKeys {
		// This is currently hardcoding the keys to ECDSA.
		key, err := r.CryptoService.Create(role, data.ECDSAKey)
		if err != nil {
			return err
		}
		if err := addKeyForRole(kdb, role, key); err != nil {
			return err
		}
	}
	for _, role := range remotelyManagedKeys {
		// This key is generated by the remote server.
		key, err := getRemoteKey(r.baseURL, r.gun, role, r.roundTrip)
		if err != nil {
			return err
		}
		logrus.Debugf("got remote %s %s key with keyID: %s",
			role, key.Algorithm(), key.ID())
		if err := addKeyForRole(kdb, role, key); err != nil {
			return err
		}
	}

	r.tufRepo = tuf.NewRepo(kdb, r.CryptoService)

	err = r.tufRepo.InitRoot(false)
	if err != nil {
		logrus.Debug("Error on InitRoot: ", err.Error())
		return err
	}
	err = r.tufRepo.InitTargets(data.CanonicalTargetsRole)
	if err != nil {
		logrus.Debug("Error on InitTargets: ", err.Error())
		return err
	}
	err = r.tufRepo.InitSnapshot()
	if err != nil {
		logrus.Debug("Error on InitSnapshot: ", err.Error())
		return err
	}

	return r.saveMetadata(serverManagesSnapshot)
}

// adds a TUF Change template to the given roles
func addChange(cl *changelist.FileChangelist, c changelist.Change, roles ...string) error {

	if len(roles) == 0 {
		roles = []string{data.CanonicalTargetsRole}
	}

	var changes []changelist.Change
	for _, role := range roles {
		role = strings.ToLower(role)

		// Ensure we can only add targets to the CanonicalTargetsRole,
		// or a Delegation role (which is <CanonicalTargetsRole>/something else)
		if role != data.CanonicalTargetsRole && !data.IsDelegation(role) {
			return data.ErrInvalidRole{
				Role:   role,
				Reason: "cannot add targets to this role",
			}
		}

		changes = append(changes, changelist.NewTufChange(
			c.Action(),
			role,
			c.Type(),
			c.Path(),
			c.Content(),
		))
	}

	for _, c := range changes {
		if err := cl.Add(c); err != nil {
			return err
		}
	}
	return nil
}

// AddDelegation creates a new changelist entry to add a delegation to the repository
// when the changelist gets applied at publish time.  This does not do any validation
// other than checking the name of the delegation to add - all that will happen
// at publish time.
func (r *NotaryRepository) AddDelegation(name string, threshold int,
	delegationKeys []data.PublicKey) error {

	if !data.IsDelegation(name) {
		return data.ErrInvalidRole{Role: name, Reason: "invalid delegation role name"}
	}

	cl, err := changelist.NewFileChangelist(filepath.Join(r.tufRepoPath, "changelist"))
	if err != nil {
		return err
	}
	defer cl.Close()

	logrus.Debugf(`Adding delegation "%s" with threshold %d, and %d keys\n`,
		name, threshold, len(delegationKeys))

	tdJSON, err := json.Marshal(&changelist.TufDelegation{
		NewThreshold: threshold,
		AddKeys:      data.KeyList(delegationKeys),
	})
	if err != nil {
		return err
	}

	template := changelist.NewTufChange(
		changelist.ActionCreate,
		name,
		changelist.TypeTargetsDelegation,
		"", // no path
		tdJSON,
	)

	return addChange(cl, template, name)
}

// RemoveDelegation creates a new changelist entry to remove a delegation from
// the repository when the changelist gets applied at publish time.
// This does not validate that the delegation exists, since one might exist
// after applying all changes.
func (r *NotaryRepository) RemoveDelegation(name string) error {

	if !data.IsDelegation(name) {
		return data.ErrInvalidRole{Role: name, Reason: "invalid delegation role name"}
	}

	cl, err := changelist.NewFileChangelist(filepath.Join(r.tufRepoPath, "changelist"))
	if err != nil {
		return err
	}
	defer cl.Close()

	logrus.Debugf(`Removing delegation "%s"\n`, name)

	template := changelist.NewTufChange(
		changelist.ActionDelete,
		name,
		changelist.TypeTargetsDelegation,
		"", // no path
		nil,
	)

	return addChange(cl, template, name)
}

// AddTarget creates new changelist entries to add a target to the given roles
// in the repository when the changelist gets appied at publish time.
// If roles are unspecified, the default role is "target".
func (r *NotaryRepository) AddTarget(target *Target, roles ...string) error {

	cl, err := changelist.NewFileChangelist(filepath.Join(r.tufRepoPath, "changelist"))
	if err != nil {
		return err
	}
	defer cl.Close()
	logrus.Debugf("Adding target \"%s\" with sha256 \"%x\" and size %d bytes.\n", target.Name, target.Hashes["sha256"], target.Length)

	meta := data.FileMeta{Length: target.Length, Hashes: target.Hashes}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	template := changelist.NewTufChange(
		changelist.ActionCreate, "", changelist.TypeTargetsTarget,
		target.Name, metaJSON)
	return addChange(cl, template, roles...)
}

// RemoveTarget creates new changelist entries to remove a target from the given
// roles in the repository when the changelist gets applied at publish time.
// If roles are unspecified, the default role is "target".
func (r *NotaryRepository) RemoveTarget(targetName string, roles ...string) error {

	cl, err := changelist.NewFileChangelist(filepath.Join(r.tufRepoPath, "changelist"))
	if err != nil {
		return err
	}
	logrus.Debugf("Removing target \"%s\"", targetName)
	template := changelist.NewTufChange(changelist.ActionDelete, "",
		changelist.TypeTargetsTarget, targetName, nil)
	return addChange(cl, template, roles...)
}

// ListTargets lists all targets for the current repository
func (r *NotaryRepository) ListTargets() ([]*Target, error) {
	c, err := r.bootstrapClient()
	if err != nil {
		return nil, err
	}

	err = c.Update()
	if err != nil {
		if err, ok := err.(signed.ErrExpired); ok {
			return nil, ErrExpired{err}
		}
		return nil, err
	}

	var targetList []*Target
	for name, meta := range r.tufRepo.Targets["targets"].Signed.Targets {
		target := &Target{Name: name, Hashes: meta.Hashes, Length: meta.Length}
		targetList = append(targetList, target)
	}

	return targetList, nil
}

// GetTargetByName returns a target given a name
func (r *NotaryRepository) GetTargetByName(name string) (*Target, error) {
	c, err := r.bootstrapClient()
	if err != nil {
		return nil, err
	}

	err = c.Update()
	if err != nil {
		if err, ok := err.(signed.ErrExpired); ok {
			return nil, ErrExpired{err}
		}
		return nil, err
	}

	meta, err := c.TargetMeta(name)
	if meta == nil {
		return nil, fmt.Errorf("No trust data for %s", name)
	} else if err != nil {
		return nil, err
	}

	return &Target{Name: name, Hashes: meta.Hashes, Length: meta.Length}, nil
}

// GetChangelist returns the list of the repository's unpublished changes
func (r *NotaryRepository) GetChangelist() (changelist.Changelist, error) {
	changelistDir := filepath.Join(r.tufRepoPath, "changelist")
	cl, err := changelist.NewFileChangelist(changelistDir)
	if err != nil {
		logrus.Debug("Error initializing changelist")
		return nil, err
	}
	return cl, nil
}

// Publish pushes the local changes in signed material to the remote notary-server
// Conceptually it performs an operation similar to a `git rebase`
func (r *NotaryRepository) Publish() error {
	var updateRoot bool
	// attempt to initialize the repo from the remote store
	c, err := r.bootstrapClient()
	if err != nil {
		if _, ok := err.(store.ErrMetaNotFound); ok {
			// if the remote store return a 404 (translated into ErrMetaNotFound),
			// there is no trust data for yet. Attempt to load it from disk.
			err := r.bootstrapRepo()
			if err != nil {
				// There are lots of reasons there might be an error, such as
				// corrupt metadata.  We need better errors from bootstrapRepo.
				logrus.Debugf("Unable to load repository from local files: %s",
					err.Error())
				return err
			}
			// We had local data but the server doesn't know about the repo yet,
			// ensure we will push the initial root file.  The root may not
			// be marked as Dirty, since there may not be any changes that
			// update it, so use a different boolean.
			updateRoot = true
		} else {
			// The remote store returned an error other than 404. We're
			// unable to determine if the repo has been initialized or not.
			logrus.Error("Could not publish Repository: ", err.Error())
			return err
		}
	} else {
		// If we were successfully able to bootstrap the client (which only pulls
		// root.json), update it with the rest of the tuf metadata in
		// preparation for applying the changelist.
		err = c.Update()
		if err != nil {
			if err, ok := err.(signed.ErrExpired); ok {
				return ErrExpired{err}
			}
			return err
		}
	}
	cl, err := r.GetChangelist()
	if err != nil {
		return err
	}
	// apply the changelist to the repo
	err = applyChangelist(r.tufRepo, cl)
	if err != nil {
		logrus.Debug("Error applying changelist")
		return err
	}

	// these are the tuf files we will need to update, serialized as JSON before
	// we send anything to remote
	updatedFiles := make(map[string][]byte)

	// check if our root file is nearing expiry. Resign if it is.
	if nearExpiry(r.tufRepo.Root) || r.tufRepo.Root.Dirty || updateRoot {
		rootJSON, err := serializeCanonicalRole(r.tufRepo, data.CanonicalRootRole)
		if err != nil {
			return err
		}
		updatedFiles[data.CanonicalRootRole] = rootJSON
	}

	// we will always re-sign targets
	targetsJSON, err := serializeCanonicalRole(r.tufRepo, data.CanonicalTargetsRole)
	if err != nil {
		return err
	}
	updatedFiles[data.CanonicalTargetsRole] = targetsJSON

	// if we initialized the repo while designating the server as the snapshot
	// signer, then there won't be a snapshots file.  However, we might now
	// have a local key (if there was a rotation), so initialize one.
	if r.tufRepo.Snapshot == nil {
		if err := r.tufRepo.InitSnapshot(); err != nil {
			return err
		}
	}

	snapshotJSON, err := serializeCanonicalRole(
		r.tufRepo, data.CanonicalSnapshotRole)

	if err == nil {
		// Only update the snapshot if we've sucessfully signed it.
		updatedFiles[data.CanonicalSnapshotRole] = snapshotJSON
	} else if _, ok := err.(signed.ErrNoKeys); ok {
		// If signing fails due to us not having the snapshot key, then
		// assume the server is going to sign, and do not include any snapshot
		// data.
		logrus.Debugf("Client does not have the key to sign snapshot. " +
			"Assuming that server should sign the snapshot.")
	} else {
		logrus.Debugf("Client was unable to sign the snapshot: %s", err.Error())
		return err
	}

	remote, err := getRemoteStore(r.baseURL, r.gun, r.roundTrip)
	if err != nil {
		return err
	}

	err = remote.SetMultiMeta(updatedFiles)
	if err != nil {
		return err
	}
	err = cl.Clear("")
	if err != nil {
		// This is not a critical problem when only a single host is pushing
		// but will cause weird behaviour if changelist cleanup is failing
		// and there are multiple hosts writing to the repo.
		logrus.Warn("Unable to clear changelist. You may want to manually delete the folder ", filepath.Join(r.tufRepoPath, "changelist"))
	}
	return nil
}

// bootstrapRepo loads the repository from the local file system.  This attempts
// to load metadata for all roles.  Since server snapshots are supported,
// if the snapshot metadata fails to load, that's ok.
// This can also be unified with some cache reading tools from tuf/client.
// This assumes that bootstrapRepo is only used by Publish()
func (r *NotaryRepository) bootstrapRepo() error {
	kdb := keys.NewDB()
	tufRepo := tuf.NewRepo(kdb, r.CryptoService)

	logrus.Debugf("Loading trusted collection.")
	rootJSON, err := r.fileStore.GetMeta("root", 0)
	if err != nil {
		return err
	}
	root := &data.SignedRoot{}
	err = json.Unmarshal(rootJSON, root)
	if err != nil {
		return err
	}
	err = tufRepo.SetRoot(root)
	if err != nil {
		return err
	}
	targetsJSON, err := r.fileStore.GetMeta("targets", 0)
	if err != nil {
		return err
	}
	targets := &data.SignedTargets{}
	err = json.Unmarshal(targetsJSON, targets)
	if err != nil {
		return err
	}
	tufRepo.SetTargets("targets", targets)

	snapshotJSON, err := r.fileStore.GetMeta("snapshot", 0)
	if err == nil {
		snapshot := &data.SignedSnapshot{}
		err = json.Unmarshal(snapshotJSON, snapshot)
		if err != nil {
			return err
		}
		tufRepo.SetSnapshot(snapshot)
	} else if _, ok := err.(store.ErrMetaNotFound); !ok {
		return err
	}

	r.tufRepo = tufRepo

	return nil
}

func (r *NotaryRepository) saveMetadata(ignoreSnapshot bool) error {
	logrus.Debugf("Saving changes to Trusted Collection.")

	rootJSON, err := serializeCanonicalRole(r.tufRepo, data.CanonicalRootRole)
	if err != nil {
		return err
	}
	err = r.fileStore.SetMeta(data.CanonicalRootRole, rootJSON)
	if err != nil {
		return err
	}

	targetsToSave := make(map[string][]byte)
	for t := range r.tufRepo.Targets {
		signedTargets, err := r.tufRepo.SignTargets(t, data.DefaultExpires("targets"))
		if err != nil {
			return err
		}
		targetsJSON, err := json.Marshal(signedTargets)
		if err != nil {
			return err
		}
		targetsToSave[t] = targetsJSON
	}

	for role, blob := range targetsToSave {
		parentDir := filepath.Dir(role)
		os.MkdirAll(parentDir, 0755)
		r.fileStore.SetMeta(role, blob)
	}

	if ignoreSnapshot {
		return nil
	}

	snapshotJSON, err := serializeCanonicalRole(r.tufRepo, data.CanonicalSnapshotRole)
	if err != nil {
		return err
	}

	return r.fileStore.SetMeta(data.CanonicalSnapshotRole, snapshotJSON)
}

func (r *NotaryRepository) bootstrapClient() (*tufclient.Client, error) {
	var rootJSON []byte
	remote, err := getRemoteStore(r.baseURL, r.gun, r.roundTrip)
	if err == nil {
		// if remote store successfully set up, try and get root from remote
		rootJSON, err = remote.GetMeta("root", maxSize)
	}

	// if remote store couldn't be setup, or we failed to get a root from it
	// load the root from cache (offline operation)
	if err != nil {
		if err, ok := err.(store.ErrMetaNotFound); ok {
			// if the error was MetaNotFound then we successfully contacted
			// the store and it doesn't know about the repo.
			return nil, err
		}
		result, cacheErr := r.fileStore.GetMeta("root", maxSize)
		if cacheErr != nil {
			// if cache didn't return a root, we cannot proceed - just return
			// the original error.
			return nil, err
		}
		rootJSON = result
		logrus.Debugf(
			"Using local cache instead of remote due to failure: %s", err.Error())
	}
	// can't just unmarshal into SignedRoot because validate root
	// needs the root.Signed field to still be []byte for signature
	// validation
	root := &data.Signed{}
	err = json.Unmarshal(rootJSON, root)
	if err != nil {
		return nil, err
	}

	err = r.CertManager.ValidateRoot(root, r.gun)
	if err != nil {
		return nil, err
	}

	kdb := keys.NewDB()
	r.tufRepo = tuf.NewRepo(kdb, r.CryptoService)

	signedRoot, err := data.RootFromSigned(root)
	if err != nil {
		return nil, err
	}
	err = r.tufRepo.SetRoot(signedRoot)
	if err != nil {
		return nil, err
	}

	return tufclient.NewClient(
		r.tufRepo,
		remote,
		kdb,
		r.fileStore,
	), nil
}

// RotateKey removes all existing keys associated with the role, and either
// creates and adds one new key or delegates managing the key to the server.
// These changes are staged in a changelist until publish is called.
func (r *NotaryRepository) RotateKey(role string, serverManagesKey bool) error {
	if role == data.CanonicalRootRole || role == data.CanonicalTimestampRole {
		return fmt.Errorf(
			"notary does not currently support rotating the %s key", role)
	}
	if serverManagesKey && role == data.CanonicalTargetsRole {
		return ErrInvalidRemoteRole{Role: data.CanonicalTargetsRole}
	}

	var (
		pubKey data.PublicKey
		err    error
	)
	if serverManagesKey {
		pubKey, err = getRemoteKey(r.baseURL, r.gun, role, r.roundTrip)
	} else {
		pubKey, err = r.CryptoService.Create(role, data.ECDSAKey)
	}
	if err != nil {
		return err
	}

	return r.rootFileKeyChange(role, changelist.ActionCreate, pubKey)
}

func (r *NotaryRepository) rootFileKeyChange(role, action string, key data.PublicKey) error {
	cl, err := changelist.NewFileChangelist(filepath.Join(r.tufRepoPath, "changelist"))
	if err != nil {
		return err
	}
	defer cl.Close()

	kl := make(data.KeyList, 0, 1)
	kl = append(kl, key)
	meta := changelist.TufRootData{
		RoleName: role,
		Keys:     kl,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	c := changelist.NewTufChange(
		action,
		changelist.ScopeRoot,
		changelist.TypeRootRole,
		role,
		metaJSON,
	)
	err = cl.Add(c)
	if err != nil {
		return err
	}
	return nil
}
