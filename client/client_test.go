package client

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	regJson "encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Sirupsen/logrus"
	ctxu "github.com/docker/distribution/context"
	"github.com/docker/go/canonical/json"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"

	"github.com/docker/notary"
	"github.com/docker/notary/client/changelist"
	"github.com/docker/notary/cryptoservice"
	"github.com/docker/notary/passphrase"
	"github.com/docker/notary/server"
	"github.com/docker/notary/server/storage"
	"github.com/docker/notary/trustmanager"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/tuf/signed"
	"github.com/docker/notary/tuf/store"
	"github.com/docker/notary/tuf/utils"
	"github.com/docker/notary/tuf/validation"
)

const password = "passphrase"

type passRoleRecorder struct {
	rolesCreated []string
	rolesAsked   []string
}

func newRoleRecorder() *passRoleRecorder {
	return &passRoleRecorder{}
}

func (p *passRoleRecorder) clear() {
	p.rolesCreated = nil
	p.rolesAsked = nil
}

func (p *passRoleRecorder) retriever(_, alias string, createNew bool, _ int) (string, bool, error) {
	if createNew {
		p.rolesCreated = append(p.rolesCreated, alias)
	} else {
		p.rolesAsked = append(p.rolesAsked, alias)
	}
	return password, false, nil
}

func (p *passRoleRecorder) compareRolesRecorded(t *testing.T, expected []string, created bool,
	args ...interface{}) {

	var actual, useExpected sort.StringSlice
	copy(expected, useExpected) // don't sort expected, since we don't want to mutate it
	sort.Stable(useExpected)

	if created {
		copy(p.rolesCreated, actual)
	} else {
		copy(p.rolesAsked, actual)
	}
	sort.Stable(actual)

	assert.Equal(t, useExpected, actual, args...)
}

// requires the following keys be created: order does not matter
func (p *passRoleRecorder) assertCreated(t *testing.T, expected []string, args ...interface{}) {
	p.compareRolesRecorded(t, expected, true, args...)
}

// requires that passwords be asked for the following keys: order does not matter
func (p *passRoleRecorder) assertAsked(t *testing.T, expected []string, args ...interface{}) {
	p.compareRolesRecorded(t, expected, false, args...)
}

var passphraseRetriever = passphrase.ConstantRetriever(password)

func simpleTestServer(t *testing.T, roles ...string) (
	*httptest.Server, *http.ServeMux, map[string]data.PrivateKey) {

	if len(roles) == 0 {
		roles = []string{data.CanonicalTimestampRole, data.CanonicalSnapshotRole}
	}
	keys := make(map[string]data.PrivateKey)
	mux := http.NewServeMux()

	for _, role := range roles {
		key, err := trustmanager.GenerateECDSAKey(rand.Reader)
		assert.NoError(t, err)

		keys[role] = key
		pubKey := data.PublicKeyFromPrivate(key)
		jsonBytes, err := json.MarshalCanonical(&pubKey)
		assert.NoError(t, err)
		keyJSON := string(jsonBytes)

		// TUF will request /v2/docker.com/notary/_trust/tuf/<role>.key
		mux.HandleFunc(
			fmt.Sprintf("/v2/docker.com/notary/_trust/tuf/%s.key", role),
			func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, keyJSON)
			})
	}

	ts := httptest.NewServer(mux)
	return ts, mux, keys
}

func fullTestServer(t *testing.T) *httptest.Server {
	// Set up server
	ctx := context.WithValue(
		context.Background(), "metaStore", storage.NewMemStorage())

	// Do not pass one of the const KeyAlgorithms here as the value! Passing a
	// string is in itself good test that we are handling it correctly as we
	// will be receiving a string from the configuration.
	ctx = context.WithValue(ctx, "keyAlgorithm", "ecdsa")

	// Eat the logs instead of spewing them out
	var b bytes.Buffer
	l := logrus.New()
	l.Out = &b
	ctx = ctxu.WithLogger(ctx, logrus.NewEntry(l))

	cryptoService := cryptoservice.NewCryptoService(
		"", trustmanager.NewKeyMemoryStore(passphraseRetriever))
	return httptest.NewServer(server.RootHandler(nil, ctx, cryptoService))
}

// server that returns some particular error code all the time
func errorTestServer(t *testing.T, errorCode int) *httptest.Server {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(errorCode)
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	return server
}

// initializes a repository in a temporary directory
func initializeRepo(t *testing.T, rootType, gun, url string,
	serverManagesSnapshot bool) (*NotaryRepository, string) {

	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	serverManagedRoles := []string{}
	if serverManagesSnapshot {
		serverManagedRoles = []string{data.CanonicalSnapshotRole}
	}

	repo, rec, rootPubKeyID := createRepoAndKey(t, rootType, tempBaseDir, gun, url)

	err = repo.Initialize(rootPubKeyID, serverManagedRoles...)
	if err != nil {
		os.RemoveAll(tempBaseDir)
	}
	assert.NoError(t, err, "error creating repository: %s", err)

	// generates the target role, maybe the snapshot role
	if serverManagesSnapshot {
		rec.assertCreated(t, []string{data.CanonicalTargetsRole})
	} else {
		rec.assertCreated(t, []string{data.CanonicalTargetsRole, data.CanonicalSnapshotRole})
	}
	// root key is cached by the cryptoservice, so when signing we don't actually ask
	// for the passphrase
	rec.assertAsked(t, nil)
	return repo, rootPubKeyID
}

// Creates a new repository and adds a root key.  Returns the repo and key ID.
func createRepoAndKey(t *testing.T, rootType, tempBaseDir, gun, url string) (
	*NotaryRepository, *passRoleRecorder, string) {

	rec := newRoleRecorder()
	repo, err := NewNotaryRepository(
		tempBaseDir, gun, url, http.DefaultTransport, rec.retriever)
	assert.NoError(t, err, "error creating repo: %s", err)

	rootPubKey, err := repo.CryptoService.Create("root", rootType)
	assert.NoError(t, err, "error generating root key: %s", err)

	rec.assertCreated(t, []string{data.CanonicalRootRole},
		"root passphrase should have been required to generate a root key")
	rec.assertAsked(t, nil)
	rec.clear()

	return repo, rec, rootPubKey.ID()
}

// creates a new notary repository with the same gun and url as the previous
// repo, in order to eliminate caches (for instance, cryptoservice cache)
// if a new directory is to be created, it also eliminates the tuf metadata
// cache
func newRepoToTestRepo(t *testing.T, existingRepo *NotaryRepository, newDir bool) (
	*NotaryRepository, *passRoleRecorder) {

	repoDir := existingRepo.baseDir
	if newDir {
		tempBaseDir, err := ioutil.TempDir("", "notary-test-")
		assert.NoError(t, err, "failed to create a temporary directory")
		repoDir = tempBaseDir
	}

	rec := newRoleRecorder()
	repo, err := NewNotaryRepository(
		repoDir, existingRepo.gun, existingRepo.baseURL,
		http.DefaultTransport, rec.retriever)
	assert.NoError(t, err, "error creating repository: %s", err)
	if err != nil && newDir {
		defer os.RemoveAll(repoDir)
	}

	return repo, rec
}

// Initializing a new repo while specifying that the server should manage the root
// role will fail.
func TestInitRepositoryManagedRolesIncludingRoot(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory")
	defer os.RemoveAll(tempBaseDir)

	repo, rec, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", "http://localhost")
	err = repo.Initialize(rootPubKeyID, data.CanonicalRootRole)
	assert.Error(t, err)
	assert.IsType(t, ErrInvalidRemoteRole{}, err)
	// Just testing the error message here in this one case
	assert.Equal(t, err.Error(),
		"notary does not support the server managing the root key")
	// no key creation happened
	rec.assertCreated(t, nil)
}

// Initializing a new repo while specifying that the server should manage some
// invalid role will fail.
func TestInitRepositoryManagedRolesInvalidRole(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory")
	defer os.RemoveAll(tempBaseDir)

	repo, rec, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", "http://localhost")
	err = repo.Initialize(rootPubKeyID, "randomrole")
	assert.Error(t, err)
	assert.IsType(t, ErrInvalidRemoteRole{}, err)
	// no key creation happened
	rec.assertCreated(t, nil)
}

// Initializing a new repo while specifying that the server should manage the
// targets role will fail.
func TestInitRepositoryManagedRolesIncludingTargets(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory")
	defer os.RemoveAll(tempBaseDir)

	repo, rec, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", "http://localhost")
	err = repo.Initialize(rootPubKeyID, data.CanonicalTargetsRole)
	assert.Error(t, err)
	assert.IsType(t, ErrInvalidRemoteRole{}, err)
	// no key creation happened
	rec.assertCreated(t, nil)
}

// Initializing a new repo while specifying that the server should manage the
// timestamp key is fine - that's what it already does, so no error.
func TestInitRepositoryManagedRolesIncludingTimestamp(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory")
	defer os.RemoveAll(tempBaseDir)

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rec, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", ts.URL)
	err = repo.Initialize(rootPubKeyID, data.CanonicalTimestampRole)
	assert.NoError(t, err)
	// generates the target role, the snapshot role
	rec.assertCreated(t, []string{data.CanonicalTargetsRole, data.CanonicalSnapshotRole})
}

// Initializing a new repo fails if unable to get the timestamp key, even if
// the snapshot key is available
func TestInitRepositoryNeedsRemoteTimestampKey(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory")
	defer os.RemoveAll(tempBaseDir)

	ts, _, _ := simpleTestServer(t, data.CanonicalSnapshotRole)
	defer ts.Close()

	repo, rec, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", ts.URL)
	err = repo.Initialize(rootPubKeyID, data.CanonicalTimestampRole)
	assert.Error(t, err)
	assert.IsType(t, store.ErrMetaNotFound{}, err)

	// locally managed keys are created first, to avoid unnecssary network calls,
	// so they would have been generated
	rec.assertCreated(t, []string{data.CanonicalTargetsRole, data.CanonicalSnapshotRole})
}

// Initializing a new repo with remote server signing fails if unable to get
// the snapshot key, even if the timestamp key is available
func TestInitRepositoryNeedsRemoteSnapshotKey(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory")
	defer os.RemoveAll(tempBaseDir)

	ts, _, _ := simpleTestServer(t, data.CanonicalTimestampRole)
	defer ts.Close()

	repo, rec, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", ts.URL)
	err = repo.Initialize(rootPubKeyID, data.CanonicalSnapshotRole)
	assert.Error(t, err)
	assert.IsType(t, store.ErrMetaNotFound{}, err)

	// locally managed keys are created first, to avoid unnecssary network calls,
	// so they would have been generated
	rec.assertCreated(t, []string{data.CanonicalTargetsRole})
}

// passing timestamp + snapshot, or just snapshot, is tested in the next two
// test cases.

// TestInitRepoServerOnlyManagesTimestampKey runs through the process of
// initializing a repository and makes sure the repository looks correct on disk.
// We test this with both an RSA and ECDSA root key.
// This test case covers the default case where the server only manages the
// timestamp key.
func TestInitRepoServerOnlyManagesTimestampKey(t *testing.T) {
	testInitRepoMetadata(t, data.ECDSAKey, false)
	testInitRepoSigningKeys(t, data.ECDSAKey, false)
	if !testing.Short() {
		testInitRepoMetadata(t, data.RSAKey, false)
		testInitRepoSigningKeys(t, data.RSAKey, false)
	}
}

// TestInitRepoServerManagesTimestampAndSnapshotKeys runs through the process of
// initializing a repository and makes sure the repository looks correct on disk.
// We test this with both an RSA and ECDSA root key.
// This test case covers the server managing both the timestap and snapshot keys.
func TestInitRepoServerManagesTimestampAndSnapshotKeys(t *testing.T) {
	testInitRepoMetadata(t, data.ECDSAKey, true)
	testInitRepoSigningKeys(t, data.ECDSAKey, true)
	if !testing.Short() {
		testInitRepoMetadata(t, data.RSAKey, true)
		testInitRepoSigningKeys(t, data.RSAKey, false)
	}
}

// This creates a new KeyFileStore in the repo's base directory and makes sure
// the repo has the right number of keys
func assertRepoHasExpectedKeys(t *testing.T, repo *NotaryRepository,
	rootKeyID string, expectedSnapshotKey bool) {

	// The repo should have a keyFileStore and have created keys using it,
	// so create a new KeyFileStore, and check that the keys do exist and are
	// valid
	ks, err := trustmanager.NewKeyFileStore(repo.baseDir, passphraseRetriever)
	assert.NoError(t, err)

	roles := make(map[string]bool)
	for keyID, role := range ks.ListKeys() {
		if role == data.CanonicalRootRole {
			assert.Equal(t, rootKeyID, keyID, "Unexpected root key ID")
		}
		// just to ensure the content of the key files created are valid
		_, r, err := ks.GetKey(keyID)
		assert.NoError(t, err)
		assert.Equal(t, role, r)
		roles[role] = true
	}
	// there is a root key and a targets key
	alwaysThere := []string{data.CanonicalRootRole, data.CanonicalTargetsRole}
	for _, role := range alwaysThere {
		_, ok := roles[role]
		assert.True(t, ok, "missing %s key", role)
	}

	// there may be a snapshots key, depending on whether the server is managing
	// the snapshots key
	_, ok := roles[data.CanonicalSnapshotRole]
	if expectedSnapshotKey {
		assert.True(t, ok, "missing snapshot key")
	} else {
		assert.False(t, ok,
			"there should be no snapshot key because the server manages it")
	}

	// The server manages the timestamp key - there should not be a timestamp
	// key
	_, ok = roles[data.CanonicalTimestampRole]
	assert.False(t, ok,
		"there should be no timestamp key because the server manages it")
}

// This creates a new certificate store in the repo's base directory and
// makes sure the repo has the right certificates
func assertRepoHasExpectedCerts(t *testing.T, repo *NotaryRepository) {
	// The repo should have a certificate store and have created certs using
	// it, so create a new store, and check that the certs do exist and
	// are valid
	trustPath := filepath.Join(repo.baseDir, notary.TrustedCertsDir)
	certStore, err := trustmanager.NewX509FilteredFileStore(
		trustPath,
		trustmanager.FilterCertsExpiredSha1,
	)
	assert.NoError(t, err)
	certificates := certStore.GetCertificates()
	assert.Len(t, certificates, 1, "unexpected number of trusted certificates")

	certID, err := trustmanager.FingerprintCert(certificates[0])
	assert.NoError(t, err, "unable to fingerprint the trusted certificate")
	assert.NotEqual(t, certID, "")
}

// Sanity check the TUF metadata files. Verify that it exists for a particular
// role, the JSON is well-formed, and the signatures exist.
// For the root.json file, also check that the root, snapshot, and
// targets key IDs are present.
func assertRepoHasExpectedMetadata(t *testing.T, repo *NotaryRepository,
	role string, expected bool) {

	filename := filepath.Join(tufDir, filepath.FromSlash(repo.gun),
		"metadata", role+".json")
	fullPath := filepath.Join(repo.baseDir, filename)
	_, err := os.Stat(fullPath)

	if expected {
		assert.NoError(t, err, "missing TUF metadata file: %s", filename)
	} else {
		assert.Error(t, err,
			"%s metadata should not exist, but does: %s", role, filename)
		return
	}

	jsonBytes, err := ioutil.ReadFile(fullPath)
	assert.NoError(t, err, "error reading TUF metadata file %s: %s", filename, err)

	var decoded data.Signed
	err = json.Unmarshal(jsonBytes, &decoded)
	assert.NoError(t, err, "error parsing TUF metadata file %s: %s", filename, err)

	assert.Len(t, decoded.Signatures, 1,
		"incorrect number of signatures in TUF metadata file %s", filename)

	assert.NotEmpty(t, decoded.Signatures[0].KeyID,
		"empty key ID field in TUF metadata file %s", filename)
	assert.NotEmpty(t, decoded.Signatures[0].Method,
		"empty method field in TUF metadata file %s", filename)
	assert.NotEmpty(t, decoded.Signatures[0].Signature,
		"empty signature in TUF metadata file %s", filename)

	// Special case for root.json: also check that the signed
	// content for keys and roles
	if role == data.CanonicalRootRole {
		var decodedRoot data.Root
		err := json.Unmarshal(decoded.Signed, &decodedRoot)
		assert.NoError(t, err, "error parsing root.json signed section: %s", err)

		assert.Equal(t, "Root", decodedRoot.Type, "_type mismatch in root.json")

		// Expect 1 key for each valid role in the Keys map - one for
		// each of root, targets, snapshot, timestamp
		assert.Len(t, decodedRoot.Keys, len(data.BaseRoles),
			"wrong number of keys in root.json")
		assert.Len(t, decodedRoot.Roles, len(data.BaseRoles),
			"wrong number of roles in root.json")

		for _, role := range data.BaseRoles {
			_, ok := decodedRoot.Roles[role]
			assert.True(t, ok, "Missing role %s in root.json", role)
		}
	}
}

func testInitRepoMetadata(t *testing.T, rootType string, serverManagesSnapshot bool) {
	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, rootType, gun, ts.URL, serverManagesSnapshot)
	defer os.RemoveAll(repo.baseDir)

	assertRepoHasExpectedKeys(t, repo, rootKeyID, !serverManagesSnapshot)
	assertRepoHasExpectedCerts(t, repo)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalRootRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTargetsRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole,
		!serverManagesSnapshot)
}

func testInitRepoSigningKeys(t *testing.T, rootType string, serverManagesSnapshot bool) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	repo, _, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", ts.URL)

	// create a new repository, so we can wipe out the cryptoservice's cached
	// keys, so we can test which keys it asks for passwords for
	repo, rec := newRepoToTestRepo(t, repo, false)

	if serverManagesSnapshot {
		err = repo.Initialize(rootPubKeyID, data.CanonicalSnapshotRole)
	} else {
		err = repo.Initialize(rootPubKeyID)
	}

	assert.NoError(t, err, "error initializing repository")

	// generates the target role, maybe the snapshot role
	if serverManagesSnapshot {
		rec.assertCreated(t, []string{data.CanonicalTargetsRole})
	} else {
		rec.assertCreated(t, []string{data.CanonicalTargetsRole, data.CanonicalSnapshotRole})
	}
	// root is asked for signing the root role
	rec.assertAsked(t, []string{data.CanonicalRootRole})
}

// TestInitRepoAttemptsExceeded tests error handling when passphrase.Retriever
// (or rather the user) insists on an incorrect password.
func TestInitRepoAttemptsExceeded(t *testing.T) {
	testInitRepoAttemptsExceeded(t, data.ECDSAKey)
	if !testing.Short() {
		testInitRepoAttemptsExceeded(t, data.RSAKey)
	}
}

func testInitRepoAttemptsExceeded(t *testing.T, rootType string) {
	gun := "docker.com/notary"
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)
	defer os.RemoveAll(tempBaseDir)

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	retriever := passphrase.ConstantRetriever("password")
	repo, err := NewNotaryRepository(tempBaseDir, gun, ts.URL, http.DefaultTransport, retriever)
	assert.NoError(t, err, "error creating repo: %s", err)
	rootPubKey, err := repo.CryptoService.Create("root", rootType)
	assert.NoError(t, err, "error generating root key: %s", err)

	retriever = passphrase.ConstantRetriever("incorrect password")
	// repo.CryptoService’s FileKeyStore caches the unlocked private key, so to test
	// private key unlocking we need a new repo instance.
	repo, err = NewNotaryRepository(tempBaseDir, gun, ts.URL, http.DefaultTransport, retriever)
	assert.NoError(t, err, "error creating repo: %s", err)
	err = repo.Initialize(rootPubKey.ID())
	assert.EqualError(t, err, trustmanager.ErrAttemptsExceeded{}.Error())
}

// TestInitRepoPasswordInvalid tests error handling when passphrase.Retriever
// (or rather the user) fails to provide a correct password.
func TestInitRepoPasswordInvalid(t *testing.T) {
	testInitRepoPasswordInvalid(t, data.ECDSAKey)
	if !testing.Short() {
		testInitRepoPasswordInvalid(t, data.RSAKey)
	}
}

func giveUpPassphraseRetriever(_, _ string, _ bool, _ int) (string, bool, error) {
	return "", true, nil
}

func testInitRepoPasswordInvalid(t *testing.T, rootType string) {
	gun := "docker.com/notary"
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)
	defer os.RemoveAll(tempBaseDir)

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	retriever := passphrase.ConstantRetriever("password")
	repo, err := NewNotaryRepository(tempBaseDir, gun, ts.URL, http.DefaultTransport, retriever)
	assert.NoError(t, err, "error creating repo: %s", err)
	rootPubKey, err := repo.CryptoService.Create("root", rootType)
	assert.NoError(t, err, "error generating root key: %s", err)

	// repo.CryptoService’s FileKeyStore caches the unlocked private key, so to test
	// private key unlocking we need a new repo instance.
	repo, err = NewNotaryRepository(tempBaseDir, gun, ts.URL, http.DefaultTransport, giveUpPassphraseRetriever)
	assert.NoError(t, err, "error creating repo: %s", err)
	err = repo.Initialize(rootPubKey.ID())
	assert.EqualError(t, err, trustmanager.ErrPasswordInvalid{}.Error())
}

func addTarget(t *testing.T, repo *NotaryRepository, targetName, targetFile string,
	roles ...string) *Target {
	target, err := NewTarget(targetName, targetFile)
	assert.NoError(t, err, "error creating target")
	err = repo.AddTarget(target, nil, roles...)
	assert.NoError(t, err, "error adding target")
	return target
}

// calls GetChangelist and gets the actual changes out
func getChanges(t *testing.T, repo *NotaryRepository) []changelist.Change {
	changeList, err := repo.GetChangelist()
	assert.NoError(t, err)
	return changeList.List()
}

// TestAddTargetToTargetRoleByDefault adds a target without specifying a role
// to a repo without delegations.  Confirms that the changelist is created
// correctly, for the targets scope.
func TestAddTargetToTargetRoleByDefault(t *testing.T) {
	testAddTargetToTargetRoleByDefault(t, false)
	testAddTargetToTargetRoleByDefault(t, true)
}

func testAddTargetToTargetRoleByDefault(t *testing.T, clearCache bool) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	var rec *passRoleRecorder
	if clearCache {
		repo, rec = newRepoToTestRepo(t, repo, false)
	}

	testAddOrDeleteTarget(t, repo, changelist.ActionCreate, nil,
		[]string{data.CanonicalTargetsRole})

	if clearCache {
		// no key creation or signing happened, because add doesn't ever require signing
		rec.assertCreated(t, nil)
		rec.assertAsked(t, nil)
	}
}

// Tests that adding a target to a repo or deleting a target from a repo,
// with the given roles, makes a change to the expected scopes
func testAddOrDeleteTarget(t *testing.T, repo *NotaryRepository, action string,
	rolesToChange []string, expectedScopes []string) {

	assert.Len(t, getChanges(t, repo), 0, "should start with zero changes")

	if action == changelist.ActionCreate {
		// Add fixtures/intermediate-ca.crt as a target. There's no particular
		// reason for using this file except that it happens to be available as
		// a fixture.
		addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt", rolesToChange...)
	} else {
		err := repo.RemoveTarget("latest", rolesToChange...)
		assert.NoError(t, err, "error removing target")
	}

	changes := getChanges(t, repo)
	assert.Len(t, changes, len(expectedScopes), "wrong number of changes files found")

	foundScopes := make(map[string]bool)
	for _, c := range changes { // there is only one
		assert.EqualValues(t, action, c.Action())
		foundScopes[c.Scope()] = true
		assert.Equal(t, "target", c.Type())
		assert.Equal(t, "latest", c.Path())
		if action == changelist.ActionCreate {
			assert.NotEmpty(t, c.Content())
		} else {
			assert.Empty(t, c.Content())
		}
	}
	assert.Len(t, foundScopes, len(expectedScopes))
	for _, expectedScope := range expectedScopes {
		_, ok := foundScopes[expectedScope]
		assert.True(t, ok, "Target was not added/removed from %s", expectedScope)
	}

	// add/delete a second time
	if action == changelist.ActionCreate {
		addTarget(t, repo, "current", "../fixtures/intermediate-ca.crt", rolesToChange...)
	} else {
		err := repo.RemoveTarget("current", rolesToChange...)
		assert.NoError(t, err, "error removing target")
	}

	changes = getChanges(t, repo)
	assert.Len(t, changes, 2*len(expectedScopes),
		"wrong number of changelist files found")

	newFileFound := false
	foundScopes = make(map[string]bool)
	for _, c := range changes {
		if c.Path() != "latest" {
			assert.EqualValues(t, action, c.Action())
			foundScopes[c.Scope()] = true
			assert.Equal(t, "target", c.Type())
			assert.Equal(t, "current", c.Path())
			if action == changelist.ActionCreate {
				assert.NotEmpty(t, c.Content())
			} else {
				assert.Empty(t, c.Content())
			}

			newFileFound = true
		}
	}
	assert.True(t, newFileFound, "second changelist file not found")
	assert.Len(t, foundScopes, len(expectedScopes))
	for _, expectedScope := range expectedScopes {
		_, ok := foundScopes[expectedScope]
		assert.True(t, ok, "Target was not added/removed from %s", expectedScope)
	}
}

// TestAddTargetToSpecifiedValidRoles adds a target to the specified roles.
// Confirms that the changelist is created correctly, one for each of the
// the specified roles as scopes.
func TestAddTargetToSpecifiedValidRoles(t *testing.T) {
	testAddTargetToSpecifiedValidRoles(t, false)
	testAddTargetToSpecifiedValidRoles(t, true)
}

func testAddTargetToSpecifiedValidRoles(t *testing.T, clearCache bool) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	var rec *passRoleRecorder
	if clearCache {
		repo, rec = newRepoToTestRepo(t, repo, false)
	}

	roleName := filepath.Join(data.CanonicalTargetsRole, "a")
	testAddOrDeleteTarget(t, repo, changelist.ActionCreate,
		[]string{
			data.CanonicalTargetsRole,
			roleName,
		},
		[]string{data.CanonicalTargetsRole, roleName})

	if clearCache {
		// no key creation or signing happened, because add doesn't ever require signing
		rec.assertCreated(t, nil)
		rec.assertAsked(t, nil)
	}
}

// TestAddTargetToSpecifiedInvalidRoles expects errors to be returned if
// adding a target to an invalid role.  If any of the roles are invalid,
// no targets are added to any roles.
func TestAddTargetToSpecifiedInvalidRoles(t *testing.T) {
	testAddTargetToSpecifiedInvalidRoles(t, false)
	testAddTargetToSpecifiedInvalidRoles(t, true)
}

func testAddTargetToSpecifiedInvalidRoles(t *testing.T, clearCache bool) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	var rec *passRoleRecorder
	if clearCache {
		repo, rec = newRepoToTestRepo(t, repo, false)
	}

	invalidRoles := []string{
		data.CanonicalRootRole,
		data.CanonicalSnapshotRole,
		data.CanonicalTimestampRole,
		"target/otherrole",
		"otherrole",
		"TARGETS/ALLCAPSROLE",
	}

	for _, invalidRole := range invalidRoles {
		target, err := NewTarget("latest", "../fixtures/intermediate-ca.crt")
		assert.NoError(t, err, "error creating target")

		err = repo.AddTarget(target, nil, data.CanonicalTargetsRole, invalidRole)
		assert.Error(t, err, "Expected an ErrInvalidRole error")
		assert.IsType(t, data.ErrInvalidRole{}, err)

		changes := getChanges(t, repo)
		assert.Len(t, changes, 0)
	}

	if clearCache {
		// no key creation or signing happened, because add doesn't ever require signing
		rec.assertCreated(t, nil)
		rec.assertAsked(t, nil)
	}
}

// General way to assert that errors writing a changefile are propagated up
func testErrorWritingChangefiles(t *testing.T, writeChangeFile func(*NotaryRepository) error) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// first, make the actual changefile unwritable by making the changelist
	// directory unwritable
	changelistPath := filepath.Join(repo.tufRepoPath, "changelist")
	err := os.MkdirAll(changelistPath, 0744)
	assert.NoError(t, err, "could not create changelist dir")
	err = os.Chmod(changelistPath, 0600)
	assert.NoError(t, err, "could not change permission of changelist dir")

	err = writeChangeFile(repo)
	assert.Error(t, err, "Expected an error writing the change")
	assert.IsType(t, &os.PathError{}, err)

	// then break prevent the changlist directory from being able to be created
	err = os.Chmod(changelistPath, 0744)
	assert.NoError(t, err, "could not change permission of temp dir")
	err = os.RemoveAll(changelistPath)
	assert.NoError(t, err, "could not remove changelist dir")
	// creating a changelist file so the directory can't be created
	err = ioutil.WriteFile(changelistPath, []byte("hi"), 0644)
	assert.NoError(t, err, "could not write temporary file")

	err = writeChangeFile(repo)
	assert.Error(t, err, "Expected an error writing the change")
	assert.IsType(t, &os.PathError{}, err)
}

// TestAddTargetErrorWritingChanges expects errors writing a change to file
// to be propagated.
func TestAddTargetErrorWritingChanges(t *testing.T) {
	testErrorWritingChangefiles(t, func(repo *NotaryRepository) error {
		target, err := NewTarget("latest", "../fixtures/intermediate-ca.crt")
		assert.NoError(t, err, "error creating target")
		return repo.AddTarget(target, nil, data.CanonicalTargetsRole)
	})
}

// TestRemoveTargetToTargetRoleByDefault removes a target without specifying a
// role from a repo.  Confirms that the changelist is created correctly for
// the targets scope.
func TestRemoveTargetToTargetRoleByDefault(t *testing.T) {
	testRemoveTargetToTargetRoleByDefault(t, false)
	testRemoveTargetToTargetRoleByDefault(t, true)
}

func testRemoveTargetToTargetRoleByDefault(t *testing.T, clearCache bool) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	var rec *passRoleRecorder
	if clearCache {
		repo, rec = newRepoToTestRepo(t, repo, false)
	}

	testAddOrDeleteTarget(t, repo, changelist.ActionDelete, nil,
		[]string{data.CanonicalTargetsRole})

	if clearCache {
		// no key creation or signing happened, because remove doesn't ever require signing
		rec.assertCreated(t, nil)
		rec.assertAsked(t, nil)
	}
}

// TestRemoveTargetFromSpecifiedValidRoles removes a target from the specified
// roles. Confirms that the changelist is created correctly, one for each of
// the the specified roles as scopes.
func TestRemoveTargetFromSpecifiedValidRoles(t *testing.T) {
	testRemoveTargetFromSpecifiedValidRoles(t, false)
	testRemoveTargetFromSpecifiedValidRoles(t, true)
}

func testRemoveTargetFromSpecifiedValidRoles(t *testing.T, clearCache bool) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	var rec *passRoleRecorder
	if clearCache {
		repo, rec = newRepoToTestRepo(t, repo, false)
	}

	roleName := filepath.Join(data.CanonicalTargetsRole, "a")
	testAddOrDeleteTarget(t, repo, changelist.ActionDelete,
		[]string{
			data.CanonicalTargetsRole,
			roleName,
		},
		[]string{data.CanonicalTargetsRole, roleName})

	if clearCache {
		// no key creation or signing happened, because remove doesn't ever require signing
		rec.assertCreated(t, nil)
		rec.assertAsked(t, nil)
	}
}

// TestRemoveTargetFromSpecifiedInvalidRoles expects errors to be returned if
// removing a target to an invalid role.  If any of the roles are invalid,
// no targets are removed from any roles.
func TestRemoveTargetToSpecifiedInvalidRoles(t *testing.T) {
	testRemoveTargetToSpecifiedInvalidRoles(t, false)
	testRemoveTargetToSpecifiedInvalidRoles(t, true)
}

func testRemoveTargetToSpecifiedInvalidRoles(t *testing.T, clearCache bool) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	var rec *passRoleRecorder
	if clearCache {
		repo, rec = newRepoToTestRepo(t, repo, false)
	}

	invalidRoles := []string{
		data.CanonicalRootRole,
		data.CanonicalSnapshotRole,
		data.CanonicalTimestampRole,
		"target/otherrole",
		"otherrole",
	}

	for _, invalidRole := range invalidRoles {
		err := repo.RemoveTarget("latest", data.CanonicalTargetsRole, invalidRole)
		assert.Error(t, err, "Expected an ErrInvalidRole error")
		assert.IsType(t, data.ErrInvalidRole{}, err)

		changes := getChanges(t, repo)
		assert.Len(t, changes, 0)
	}

	if clearCache {
		// no key creation or signing happened, because remove doesn't ever require signing
		rec.assertCreated(t, nil)
		rec.assertAsked(t, nil)
	}
}

// TestRemoveTargetErrorWritingChanges expects errors writing a change to file
// to be propagated.
func TestRemoveTargetErrorWritingChanges(t *testing.T) {
	testErrorWritingChangefiles(t, func(repo *NotaryRepository) error {
		return repo.RemoveTarget("latest", data.CanonicalTargetsRole)
	})
}

// TestListTarget fakes serving signed metadata files over the test's
// internal HTTP server to ensure that ListTargets returns the correct number
// of listed targets.
// We test this with both an RSA and ECDSA root key
func TestListTarget(t *testing.T) {
	testListEmptyTargets(t, data.ECDSAKey)
	testListTarget(t, data.ECDSAKey)
	testListTargetWithDelegates(t, data.ECDSAKey)
	if !testing.Short() {
		testListEmptyTargets(t, data.RSAKey)
		testListTarget(t, data.RSAKey)
		testListTargetWithDelegates(t, data.RSAKey)
	}
}

func testListEmptyTargets(t *testing.T, rootType string) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	_, err := repo.ListTargets(data.CanonicalTargetsRole)
	assert.Error(t, err) // no trust data
}

// reads data from the repository in order to fake data being served via
// the ServeMux.
func fakeServerData(t *testing.T, repo *NotaryRepository, mux *http.ServeMux,
	keys map[string]data.PrivateKey) {

	timestampKey, ok := keys[data.CanonicalTimestampRole]
	assert.True(t, ok)
	savedTUFRepo := repo.tufRepo // in case this is overwritten

	fileStore, err := trustmanager.NewKeyFileStore(repo.baseDir, passphraseRetriever)
	assert.NoError(t, err)
	fileStore.AddKey(
		filepath.Join(filepath.FromSlash(repo.gun), timestampKey.ID()),
		"nonroot", timestampKey)

	rootJSONFile := filepath.Join(repo.baseDir, "tuf",
		filepath.FromSlash(repo.gun), "metadata", "root.json")
	rootFileBytes, err := ioutil.ReadFile(rootJSONFile)

	signedTargets, err := savedTUFRepo.SignTargets(
		"targets", data.DefaultExpires("targets"))
	assert.NoError(t, err)

	signedLevel1, err := savedTUFRepo.SignTargets(
		"targets/level1",
		data.DefaultExpires(data.CanonicalTargetsRole),
	)
	if _, ok := savedTUFRepo.Targets["targets/level1"]; ok {
		assert.NoError(t, err)
	}

	signedLevel2, err := savedTUFRepo.SignTargets(
		"targets/level2",
		data.DefaultExpires(data.CanonicalTargetsRole),
	)
	if _, ok := savedTUFRepo.Targets["targets/level2"]; ok {
		assert.NoError(t, err)
	}

	nested, err := savedTUFRepo.SignTargets(
		"targets/level1/level2",
		data.DefaultExpires(data.CanonicalTargetsRole),
	)

	if _, ok := savedTUFRepo.Targets["targets/level1/level2"]; ok {
		assert.NoError(t, err)
	}

	signedSnapshot, err := savedTUFRepo.SignSnapshot(
		data.DefaultExpires("snapshot"))
	assert.NoError(t, err)

	signedTimestamp, err := savedTUFRepo.SignTimestamp(
		data.DefaultExpires("timestamp"))
	assert.NoError(t, err)

	timestampJSON, _ := json.Marshal(signedTimestamp)
	snapshotJSON, _ := json.Marshal(signedSnapshot)
	targetsJSON, _ := json.Marshal(signedTargets)
	level1JSON, _ := json.Marshal(signedLevel1)
	level2JSON, _ := json.Marshal(signedLevel2)
	nestedJSON, _ := json.Marshal(nested)

	cksmBytes := sha256.Sum256(rootFileBytes)
	rootChecksum := hex.EncodeToString(cksmBytes[:])

	cksmBytes = sha256.Sum256(snapshotJSON)
	snapshotChecksum := hex.EncodeToString(cksmBytes[:])

	cksmBytes = sha256.Sum256(targetsJSON)
	targetsChecksum := hex.EncodeToString(cksmBytes[:])

	cksmBytes = sha256.Sum256(level1JSON)
	level1Checksum := hex.EncodeToString(cksmBytes[:])

	cksmBytes = sha256.Sum256(level2JSON)
	level2Checksum := hex.EncodeToString(cksmBytes[:])

	cksmBytes = sha256.Sum256(nestedJSON)
	nestedChecksum := hex.EncodeToString(cksmBytes[:])

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/root.json",
		func(w http.ResponseWriter, r *http.Request) {
			assert.NoError(t, err)
			fmt.Fprint(w, string(rootFileBytes))
		})
	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/root."+rootChecksum+".json",
		func(w http.ResponseWriter, r *http.Request) {
			assert.NoError(t, err)
			fmt.Fprint(w, string(rootFileBytes))
		})

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/timestamp.json",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(timestampJSON))
		})

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/snapshot.json",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(snapshotJSON))
		})
	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/snapshot."+snapshotChecksum+".json",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(snapshotJSON))
		})

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/targets.json",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(targetsJSON))
		})
	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/targets."+targetsChecksum+".json",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(targetsJSON))
		})

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/targets/level1.json",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(level1JSON))
		})
	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/targets/level1."+level1Checksum+".json",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(level1JSON))
		})

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/targets/level2.json",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(level2JSON))
		})
	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/targets/level2."+level2Checksum+".json",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(level2JSON))
		})
	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/targets/level1/level2."+nestedChecksum+".json",
		func(w http.ResponseWriter, r *http.Request) {
			level2JSON, err := json.Marshal(nested)
			assert.NoError(t, err)
			fmt.Fprint(w, string(level2JSON))
		})
}

// We want to sort by name, so we can guarantee ordering.
type targetSorter []*TargetWithRole

func (k targetSorter) Len() int           { return len(k) }
func (k targetSorter) Swap(i, j int)      { k[i], k[j] = k[j], k[i] }
func (k targetSorter) Less(i, j int) bool { return k[i].Name < k[j].Name }

func testListTarget(t *testing.T, rootType string) {
	ts, mux, keys := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// tests need to manually bootstrap timestamp as client doesn't generate it
	err := repo.tufRepo.InitTimestamp()
	assert.NoError(t, err, "error creating repository: %s", err)

	latestTarget := addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt")
	currentTarget := addTarget(t, repo, "current", "../fixtures/intermediate-ca.crt")

	// Apply the changelist. Normally, this would be done by Publish

	// load the changelist for this repo
	cl, err := changelist.NewFileChangelist(
		filepath.Join(repo.baseDir, "tuf", filepath.FromSlash(repo.gun), "changelist"))
	assert.NoError(t, err, "could not open changelist")

	// apply the changelist to the repo
	err = applyChangelist(repo.tufRepo, cl)
	assert.NoError(t, err, "could not apply changelist")

	fakeServerData(t, repo, mux, keys)

	targets, err := repo.ListTargets(data.CanonicalTargetsRole)
	assert.NoError(t, err)

	// Should be two targets
	assert.Len(t, targets, 2, "unexpected number of targets returned by ListTargets")

	sort.Stable(targetSorter(targets))

	// the targets should both be found in the targets role
	for _, foundTarget := range targets {
		assert.Equal(t, data.CanonicalTargetsRole, foundTarget.Role)
	}

	// current should be first
	assert.True(t, reflect.DeepEqual(*currentTarget, targets[0].Target), "current target does not match")
	assert.True(t, reflect.DeepEqual(*latestTarget, targets[1].Target), "latest target does not match")

	// Also test GetTargetByName
	newLatestTarget, err := repo.GetTargetByName("latest")
	assert.NoError(t, err)
	assert.Equal(t, data.CanonicalTargetsRole, newLatestTarget.Role)
	assert.True(t, reflect.DeepEqual(*latestTarget, newLatestTarget.Target), "latest target does not match")

	newCurrentTarget, err := repo.GetTargetByName("current")
	assert.NoError(t, err)
	assert.Equal(t, data.CanonicalTargetsRole, newCurrentTarget.Role)
	assert.True(t, reflect.DeepEqual(*currentTarget, newCurrentTarget.Target), "current target does not match")
}

func testListTargetWithDelegates(t *testing.T, rootType string) {
	ts, mux, keys := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// tests need to manually bootstrap timestamp as client doesn't generate it
	err := repo.tufRepo.InitTimestamp()
	assert.NoError(t, err, "error creating repository: %s", err)

	latestTarget := addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt")
	currentTarget := addTarget(t, repo, "current", "../fixtures/intermediate-ca.crt")

	// setup delegated targets/level1 role
	k, err := repo.CryptoService.Create("targets/level1", rootType)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationKeys("targets/level1", []data.PublicKey{k}, []string{}, 1)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationPaths("targets/level1", []string{""}, []string{}, false)
	assert.NoError(t, err)
	delegatedTarget := addTarget(t, repo, "current", "../fixtures/root-ca.crt", "targets/level1")
	otherTarget := addTarget(t, repo, "other", "../fixtures/root-ca.crt", "targets/level1")

	// setup delegated targets/level2 role
	k, err = repo.CryptoService.Create("targets/level2", rootType)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationKeys("targets/level2", []data.PublicKey{k}, []string{}, 1)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationPaths("targets/level2", []string{""}, []string{}, false)
	assert.NoError(t, err)
	// this target should not show up as the one in targets/level1 takes higher priority
	_ = addTarget(t, repo, "current", "../fixtures/notary-server.crt", "targets/level2")
	// this target should show up as the name doesn't exist elsewhere
	level2Target := addTarget(t, repo, "level2", "../fixtures/notary-server.crt", "targets/level2")

	// Apply the changelist. Normally, this would be done by Publish

	// load the changelist for this repo
	cl, err := changelist.NewFileChangelist(
		filepath.Join(repo.baseDir, "tuf", filepath.FromSlash(repo.gun), "changelist"))
	assert.NoError(t, err, "could not open changelist")

	// apply the changelist to the repo, then clear it
	err = applyChangelist(repo.tufRepo, cl)
	assert.NoError(t, err, "could not apply changelist")
	assert.NoError(t, cl.Clear(""))

	_, ok := repo.tufRepo.Targets["targets/level1"].Signed.Targets["current"]
	assert.True(t, ok)
	_, ok = repo.tufRepo.Targets["targets/level1"].Signed.Targets["other"]
	assert.True(t, ok)
	_, ok = repo.tufRepo.Targets["targets/level2"].Signed.Targets["level2"]
	assert.True(t, ok)

	// setup delegated targets/level1/level2 role separately, which can only modify paths prefixed with "level2"
	// This is done separately due to target shadowing
	k, err = repo.CryptoService.Create("targets/level1/level2", rootType)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationKeys("targets/level1/level2", []data.PublicKey{k}, []string{}, 1)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationPaths("targets/level1/level2", []string{"level2"}, []string{}, false)
	assert.NoError(t, err)
	nestedTarget := addTarget(t, repo, "level2", "../fixtures/notary-signer.crt", "targets/level1/level2")
	// load the changelist for this repo
	cl, err = changelist.NewFileChangelist(
		filepath.Join(repo.baseDir, "tuf", filepath.FromSlash(repo.gun), "changelist"))
	assert.NoError(t, err, "could not open changelist")
	// apply the changelist to the repo
	err = applyChangelist(repo.tufRepo, cl)
	assert.NoError(t, err, "could not apply changelist")
	// check the changelist was applied
	_, ok = repo.tufRepo.Targets["targets/level1/level2"].Signed.Targets["level2"]
	assert.True(t, ok)

	fakeServerData(t, repo, mux, keys)

	// test default listing
	targets, err := repo.ListTargets()
	assert.NoError(t, err)

	// Should be four targets
	assert.Len(t, targets, 4, "unexpected number of targets returned by ListTargets")

	sort.Stable(targetSorter(targets))

	// current should be first.
	assert.True(t, reflect.DeepEqual(*currentTarget, targets[0].Target), "current target does not match")
	assert.Equal(t, data.CanonicalTargetsRole, targets[0].Role)

	assert.True(t, reflect.DeepEqual(*latestTarget, targets[1].Target), "latest target does not match")
	assert.Equal(t, data.CanonicalTargetsRole, targets[1].Role)

	// This target shadows the "level2" target in level1/level2
	assert.True(t, reflect.DeepEqual(*level2Target, targets[2].Target), "level2 target does not match")
	assert.Equal(t, "targets/level2", targets[2].Role)

	assert.True(t, reflect.DeepEqual(*otherTarget, targets[3].Target), "other target does not match")
	assert.Equal(t, "targets/level1", targets[3].Role)

	// test listing with priority specified
	targets, err = repo.ListTargets("targets/level1", data.CanonicalTargetsRole)
	assert.NoError(t, err)

	// Should be four targets
	assert.Len(t, targets, 4, "unexpected number of targets returned by ListTargets")

	sort.Stable(targetSorter(targets))

	// current (in delegated role) should be first
	assert.True(t, reflect.DeepEqual(*delegatedTarget, targets[0].Target), "current target does not match")
	assert.Equal(t, "targets/level1", targets[0].Role)

	assert.True(t, reflect.DeepEqual(*latestTarget, targets[1].Target), "latest target does not match")
	assert.Equal(t, data.CanonicalTargetsRole, targets[1].Role)

	// Now the level1/level2 target shadows the level2 target
	assert.True(t, reflect.DeepEqual(*nestedTarget, targets[2].Target), "level1/level2 target does not match")
	assert.Equal(t, "targets/level1/level2", targets[2].Role)

	assert.True(t, reflect.DeepEqual(*otherTarget, targets[3].Target), "other target does not match")
	assert.Equal(t, "targets/level1", targets[3].Role)

	// Also test GetTargetByName
	newLatestTarget, err := repo.GetTargetByName("latest")
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(*latestTarget, newLatestTarget.Target), "latest target does not match")
	assert.Equal(t, data.CanonicalTargetsRole, newLatestTarget.Role)

	newCurrentTarget, err := repo.GetTargetByName("current", "targets/level1", "targets")
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(*delegatedTarget, newCurrentTarget.Target), "current target does not match")
	assert.Equal(t, "targets/level1", newCurrentTarget.Role)

	newOtherTarget, err := repo.GetTargetByName("other")
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(*otherTarget, newOtherTarget.Target), "other target does not match")
	assert.Equal(t, "targets/level1", newOtherTarget.Role)

	newLevel2Target, err := repo.GetTargetByName("level2")
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(*level2Target, newLevel2Target.Target), "level2 target does not match")
	assert.Equal(t, "targets/level2", newLevel2Target.Role)

	// Shadow by prioritizing level1, but exclude level1/level2, so we should still get targets/level2's level2 target
	newLevel2Target, err = repo.GetTargetByName("level2", "targets/level1", "targets/level2", "targets/level1/level2")
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(*level2Target, newLevel2Target.Target), "level2 target does not match")
	assert.Equal(t, "targets/level2", newLevel2Target.Role)

	// Prioritize level1 to get level1/level2's level2 target
	newLevel2Target, err = repo.GetTargetByName("level2", "targets/level1")
	assert.NoError(t, err)
	assert.True(t, reflect.DeepEqual(*nestedTarget, newLevel2Target.Target), "level2 target does not match")
	assert.Equal(t, "targets/level1/level2", newLevel2Target.Role)
}

func TestListTargetRestrictsDelegationPaths(t *testing.T) {
	ts, mux, keys := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// tests need to manually bootstrap timestamp as client doesn't generate it
	err := repo.tufRepo.InitTimestamp()
	assert.NoError(t, err, "error creating repository: %s", err)

	// setup delegated targets/level1 role
	k, err := repo.CryptoService.Create("targets/level1", data.ECDSAKey)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationKeys("targets/level1", []data.PublicKey{k}, []string{}, 1)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationPaths("targets/level1", []string{""}, []string{}, false)
	assert.NoError(t, err)
	addTarget(t, repo, "level1-target", "../fixtures/root-ca.crt", "targets/level1")
	addTarget(t, repo, "incorrectly-named-target", "../fixtures/root-ca.crt", "targets/level1")

	// setup delegated targets/level2 role
	err = repo.tufRepo.UpdateDelegationKeys("targets/level1/level2", []data.PublicKey{k}, []string{}, 1)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationPaths("targets/level1/level2", []string{""}, []string{}, false)
	assert.NoError(t, err)
	addTarget(t, repo, "level2-target", "../fixtures/notary-server.crt", "targets/level1/level2")
	addTarget(t, repo, "level1-level2-target", "../fixtures/notary-server.crt", "targets/level1/level2")

	// Apply the changelist. Normally, this would be done by Publish

	// load the changelist for this repo
	cl, err := changelist.NewFileChangelist(
		filepath.Join(repo.baseDir, "tuf", filepath.FromSlash(repo.gun), "changelist"))
	assert.NoError(t, err, "could not open changelist")

	// apply the changelist to the repo
	err = applyChangelist(repo.tufRepo, cl)
	assert.NoError(t, err, "could not apply changelist")

	assert.NoError(t, cl.Clear(""))

	// Now restrict the paths
	err = repo.tufRepo.UpdateDelegationPaths("targets/level1", []string{"level1"}, []string{}, false)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationPaths("targets/level1/level2", []string{"level1-level2", "level2"}, []string{}, false)
	assert.NoError(t, err)

	err = repo.tufRepo.UpdateDelegationPaths("targets/level1", []string{}, []string{""}, false)
	assert.NoError(t, err)
	err = repo.tufRepo.UpdateDelegationPaths("targets/level1/level2", []string{}, []string{""}, false)
	assert.NoError(t, err)

	// load the changelist for this repo
	cl, err = changelist.NewFileChangelist(
		filepath.Join(repo.baseDir, "tuf", filepath.FromSlash(repo.gun), "changelist"))
	assert.NoError(t, err, "could not open changelist")

	// apply the changelist to the repo
	err = applyChangelist(repo.tufRepo, cl)
	assert.NoError(t, err, "could not apply changelist")

	fakeServerData(t, repo, mux, keys)

	// test default listing
	targets, err := repo.ListTargets("targets/level1")
	assert.NoError(t, err)

	// Should be four targets
	assert.Len(t, targets, 2, "unexpected number of targets returned by ListTargets")

	sort.Stable(targetSorter(targets))

	var foundLevel1, foundLevel2 bool

	for _, tgts := range targets {
		switch tgts.Name {
		case "level1-target":
			assert.Equal(t, "targets/level1", tgts.Role)
			foundLevel1 = true
		case "level1-level2-target":
			assert.Equal(t, "targets/level1/level2", tgts.Role)
			foundLevel2 = true
		}
	}

	assert.True(t, foundLevel1)
	assert.True(t, foundLevel2)

	// test GetTargetByName
	tgt, err := repo.GetTargetByName("level1-target", "targets/level1")
	assert.NoError(t, err)
	assert.NotNil(t, tgt)
	assert.Equal(t, tgt.Role, "targets/level1")

	tgt, err = repo.GetTargetByName("level1-level2-target", "targets/level1")
	assert.NoError(t, err)
	assert.NotNil(t, tgt)
	assert.Equal(t, tgt.Role, "targets/level1/level2")

	tgt, err = repo.GetTargetByName("level2-target", "targets/level1/level2")
	assert.Error(t, err)
	assert.Nil(t, tgt)
}

// TestValidateRootKey verifies that the public data in root.json for the root
// key is a valid x509 certificate.
func TestValidateRootKey(t *testing.T) {
	testValidateRootKey(t, data.ECDSAKey)
	if !testing.Short() {
		testValidateRootKey(t, data.RSAKey)
	}
}

func testValidateRootKey(t *testing.T, rootType string) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	rootJSONFile := filepath.Join(repo.baseDir, "tuf", filepath.FromSlash(repo.gun),
		"metadata", "root.json")

	jsonBytes, err := ioutil.ReadFile(rootJSONFile)
	assert.NoError(t, err, "error reading TUF metadata file %s: %s", rootJSONFile, err)

	var decoded data.Signed
	err = json.Unmarshal(jsonBytes, &decoded)
	assert.NoError(t, err, "error parsing TUF metadata file %s: %s", rootJSONFile, err)

	var decodedRoot data.Root
	err = json.Unmarshal(decoded.Signed, &decodedRoot)
	assert.NoError(t, err, "error parsing root.json signed section: %s", err)

	keyids := []string{}
	for role, roleData := range decodedRoot.Roles {
		if role == "root" {
			keyids = append(keyids, roleData.KeyIDs...)
		}
	}
	assert.NotEmpty(t, keyids)

	for _, keyid := range keyids {
		key, ok := decodedRoot.Keys[keyid]
		assert.True(t, ok, "key id not found in keys")
		_, err := trustmanager.LoadCertFromPEM(key.Public())
		assert.NoError(t, err, "key is not a valid cert")
	}
}

// TestGetChangelist ensures that the changelist returned matches the changes
// added.
// We test this with both an RSA and ECDSA root key
func TestGetChangelist(t *testing.T) {
	testGetChangelist(t, data.ECDSAKey)
	if !testing.Short() {
		testGetChangelist(t, data.RSAKey)
	}
}

func testGetChangelist(t *testing.T, rootType string) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	assert.Len(t, getChanges(t, repo), 0, "No changes should be in changelist yet")

	// Create 2 targets
	addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt")
	addTarget(t, repo, "current", "../fixtures/intermediate-ca.crt")

	// Test loading changelist
	chgs := getChanges(t, repo)
	assert.Len(t, chgs, 2, "Wrong number of changes returned from changelist")

	changes := make(map[string]changelist.Change)
	for _, ch := range chgs {
		changes[ch.Path()] = ch
	}

	currentChange := changes["current"]
	assert.NotNil(t, currentChange, "Expected changelist to contain a change for path 'current'")
	assert.EqualValues(t, changelist.ActionCreate, currentChange.Action())
	assert.Equal(t, "targets", currentChange.Scope())
	assert.Equal(t, "target", currentChange.Type())
	assert.Equal(t, "current", currentChange.Path())

	latestChange := changes["latest"]
	assert.NotNil(t, latestChange, "Expected changelist to contain a change for path 'latest'")
	assert.EqualValues(t, changelist.ActionCreate, latestChange.Action())
	assert.Equal(t, "targets", latestChange.Scope())
	assert.Equal(t, "target", latestChange.Type())
	assert.Equal(t, "latest", latestChange.Path())
}

// Create a repo, instantiate a notary server, and publish the bare repo to the
// server, signing all the non-timestamp metadata.  Root, targets, and snapshots
// (if locally signing) should be sent.
func TestPublishBareRepo(t *testing.T) {
	testPublishNoData(t, data.ECDSAKey, false, true)
	testPublishNoData(t, data.ECDSAKey, false, false)
	testPublishNoData(t, data.ECDSAKey, true, true)
	testPublishNoData(t, data.ECDSAKey, true, false)
	if !testing.Short() {
		testPublishNoData(t, data.RSAKey, false, true)
		testPublishNoData(t, data.RSAKey, false, false)
		testPublishNoData(t, data.RSAKey, true, true)
		testPublishNoData(t, data.RSAKey, true, false)
	}
}

func testPublishNoData(t *testing.T, rootType string, clearCache, serverManagesSnapshot bool) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo1, _ := initializeRepo(t, rootType, "docker.com/notary", ts.URL,
		serverManagesSnapshot)
	defer os.RemoveAll(repo1.baseDir)

	var rec *passRoleRecorder
	if clearCache {
		rec = newRoleRecorder()
		repo1, rec = newRepoToTestRepo(t, repo1, false)
	}

	assert.NoError(t, repo1.Publish())

	if clearCache {
		// signing is only done by the target/snapshot keys
		rec.assertCreated(t, nil)
		if serverManagesSnapshot {
			rec.assertAsked(t, []string{data.CanonicalTargetsRole})
		} else {
			rec.assertAsked(t, []string{data.CanonicalTargetsRole, data.CanonicalSnapshotRole})
		}
	}

	// use another repo to check metadata
	repo2, _ := newRepoToTestRepo(t, repo1, true)
	defer os.RemoveAll(repo2.baseDir)

	targets, err := repo2.ListTargets()
	assert.NoError(t, err)
	assert.Empty(t, targets)

	for _, role := range data.BaseRoles {
		// we don't cache timstamp metadata
		if role != data.CanonicalTimestampRole {
			assertRepoHasExpectedMetadata(t, repo2, role, true)
		}
	}
}

// Publishing an uninitialized repo will fail, but initializing and republishing
// after should succeed
func TestPublishUninitializedRepo(t *testing.T) {
	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	// uninitialized repo should fail to publish
	tempBaseDir, err := ioutil.TempDir("", "notary-tests")
	assert.NoError(t, err)
	defer os.RemoveAll(tempBaseDir)

	repo, err := NewNotaryRepository(tempBaseDir, gun, ts.URL,
		http.DefaultTransport, passphraseRetriever)
	assert.NoError(t, err, "error creating repository: %s", err)
	err = repo.Publish()
	assert.Error(t, err)

	// no metadata created
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalRootRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTargetsRole, false)

	// now, initialize and republish in the same directory
	rootPubKey, err := repo.CryptoService.Create("root", data.ECDSAKey)
	assert.NoError(t, err, "error generating root key: %s", err)

	assert.NoError(t, repo.Initialize(rootPubKey.ID()))

	// now metadata is created
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalRootRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTargetsRole, true)

	assert.NoError(t, repo.Publish())
}

// Create a repo, instantiate a notary server, and publish the repo with
// some targets to the server, signing all the non-timestamp metadata.
// We test this with both an RSA and ECDSA root key
func TestPublishClientHasSnapshotKey(t *testing.T) {
	testPublishWithData(t, data.ECDSAKey, true, false)
	testPublishWithData(t, data.ECDSAKey, false, false)
	if !testing.Short() {
		testPublishWithData(t, data.RSAKey, true, false)
		testPublishWithData(t, data.RSAKey, false, false)
	}
}

// Create a repo, instantiate a notary server (designating the server as the
// snapshot signer) , and publish the repo with some targets to the server,
// signing the root and targets metadata only.  The server should sign just fine.
// We test this with both an RSA and ECDSA root key
func TestPublishAfterInitServerHasSnapshotKey(t *testing.T) {
	testPublishWithData(t, data.ECDSAKey, true, true)
	testPublishWithData(t, data.ECDSAKey, false, true)
	if !testing.Short() {
		testPublishWithData(t, data.RSAKey, true, true)
		testPublishWithData(t, data.RSAKey, false, true)
	}
}

func testPublishWithData(t *testing.T, rootType string, clearCache, serverManagesSnapshot bool) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, "docker.com/notary", ts.URL,
		serverManagesSnapshot)
	defer os.RemoveAll(repo.baseDir)

	var rec *passRoleRecorder
	if clearCache {
		rec = newRoleRecorder()
		repo, rec = newRepoToTestRepo(t, repo, false)
	}

	assertPublishToRolesSucceeds(t, repo, nil, []string{data.CanonicalTargetsRole})

	if clearCache {
		// signing is only done by the target/snapshot keys
		rec.assertCreated(t, nil)
		if serverManagesSnapshot {
			rec.assertAsked(t, []string{data.CanonicalTargetsRole})
		} else {
			rec.assertAsked(t, []string{data.CanonicalTargetsRole, data.CanonicalSnapshotRole})
		}
	}
}

// asserts that adding to the given roles results in the targets actually being
// added only to the expected roles and no others
func assertPublishToRolesSucceeds(t *testing.T, repo1 *NotaryRepository,
	publishToRoles []string, expectedPublishedRoles []string) {

	// were there unpublished changes before?
	changesOffset := len(getChanges(t, repo1))

	// Create 2 targets - (actually 3, but we delete 1)
	addTarget(t, repo1, "toDelete", "../fixtures/intermediate-ca.crt", publishToRoles...)
	latestTarget := addTarget(
		t, repo1, "latest", "../fixtures/intermediate-ca.crt", publishToRoles...)
	currentTarget := addTarget(
		t, repo1, "current", "../fixtures/intermediate-ca.crt", publishToRoles...)
	repo1.RemoveTarget("toDelete", publishToRoles...)

	// if no roles are provided, the default role is target
	numRoles := int(math.Max(1, float64(len(publishToRoles))))
	assert.Len(t, getChanges(t, repo1), changesOffset+4*numRoles,
		"wrong number of changelist files found")

	// Now test Publish
	err := repo1.Publish()
	assert.NoError(t, err)
	assert.Len(t, getChanges(t, repo1), 0, "wrong number of changelist files found")

	// use another repo to check metadata
	repo2, _ := newRepoToTestRepo(t, repo1, true)
	defer os.RemoveAll(repo2.baseDir)

	// Should be two targets per role
	for _, role := range expectedPublishedRoles {
		for _, repo := range []*NotaryRepository{repo1, repo2} {
			targets, err := repo.ListTargets(role)
			assert.NoError(t, err)

			assert.Len(t, targets, 2,
				"unexpected number of targets returned by ListTargets(%s)", role)

			sort.Stable(targetSorter(targets))

			assert.True(t, reflect.DeepEqual(*currentTarget, targets[0].Target), "current target does not match")
			assert.Equal(t, role, targets[0].Role)
			assert.True(t, reflect.DeepEqual(*latestTarget, targets[1].Target), "latest target does not match")
			assert.Equal(t, role, targets[1].Role)

			// Also test GetTargetByName
			newLatestTarget, err := repo.GetTargetByName("latest", role)
			assert.NoError(t, err)
			assert.True(t, reflect.DeepEqual(*latestTarget, newLatestTarget.Target), "latest target does not match")
			assert.Equal(t, role, newLatestTarget.Role)

			newCurrentTarget, err := repo.GetTargetByName("current", role)
			assert.NoError(t, err)
			assert.True(t, reflect.DeepEqual(*currentTarget, newCurrentTarget.Target), "current target does not match")
			assert.Equal(t, role, newCurrentTarget.Role)
		}
	}
}

// After pulling a repo from the server, so there is a snapshots metadata file,
// push a different target to the server (the server is still the snapshot
// signer).  The server should sign just fine.
// We test this with both an RSA and ECDSA root key
func TestPublishAfterPullServerHasSnapshotKey(t *testing.T) {
	testPublishAfterPullServerHasSnapshotKey(t, data.ECDSAKey)
	if !testing.Short() {
		testPublishAfterPullServerHasSnapshotKey(t, data.RSAKey)
	}
}

func testPublishAfterPullServerHasSnapshotKey(t *testing.T, rootType string) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, "docker.com/notary", ts.URL, true)
	defer os.RemoveAll(repo.baseDir)
	// no timestamp metadata because that comes from the server
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTimestampRole, false)
	// no snapshot metadata because that comes from the server
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, false)

	// Publish something
	published := addTarget(t, repo, "v1", "../fixtures/intermediate-ca.crt")
	assert.NoError(t, repo.Publish())

	// still no timestamp or snapshot metadata info
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTimestampRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, false)

	// list, so that the snapshot metadata is pulled from server
	targets, err := repo.ListTargets(data.CanonicalTargetsRole)
	assert.NoError(t, err)
	assert.Equal(t, []*TargetWithRole{{Target: *published, Role: data.CanonicalTargetsRole}}, targets)
	// listing downloaded the timestamp and snapshot metadata info
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTimestampRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, true)

	// Publish again should succeed
	addTarget(t, repo, "v2", "../fixtures/intermediate-ca.crt")
	err = repo.Publish()
	assert.NoError(t, err)
}

// If neither the client nor the server has the snapshot key, signing will fail
// with an ErrNoKeys error.
// We test this with both an RSA and ECDSA root key
func TestPublishNoOneHasSnapshotKey(t *testing.T) {
	testPublishNoOneHasSnapshotKey(t, data.ECDSAKey)
	if !testing.Short() {
		testPublishNoOneHasSnapshotKey(t, data.RSAKey)
	}
}

func testPublishNoOneHasSnapshotKey(t *testing.T, rootType string) {
	ts := fullTestServer(t)
	defer ts.Close()

	// create repo and delete the snapshot key and metadata
	repo, _ := initializeRepo(t, rootType, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	snapshotRole, ok := repo.tufRepo.Root.Signed.Roles[data.CanonicalSnapshotRole]
	assert.True(t, ok)
	for _, keyID := range snapshotRole.KeyIDs {
		repo.CryptoService.RemoveKey(keyID)
	}

	// Publish something
	addTarget(t, repo, "v1", "../fixtures/intermediate-ca.crt")
	err := repo.Publish()
	assert.Error(t, err)
	assert.IsType(t, validation.ErrBadHierarchy{}, err)
}

// If the snapshot metadata is corrupt or the snapshot metadata is unreadable,
// we can't publish for the first time (whether the client or server has the
// snapshot key), because there is no existing data for us to download. If the
// repo has already been published, it doesn't matter if the metadata is corrupt
// because we can just redownload if it is.
func TestPublishSnapshotCorrupt(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	// do not publish first - publish should fail with corrupt snapshot data even with server signing snapshot
	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary1", ts.URL, true)
	defer os.RemoveAll(repo.baseDir)
	testPublishBadMetadata(t, data.CanonicalSnapshotRole, repo, false, false)

	// do not publish first - publish should fail with corrupt snapshot data with local snapshot signing
	repo, _ = initializeRepo(t, data.ECDSAKey, "docker.com/notary2", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	testPublishBadMetadata(t, data.CanonicalSnapshotRole, repo, false, false)

	// publish first - publish again should succeed despite corrupt snapshot data (server signing snapshot)
	repo, _ = initializeRepo(t, data.ECDSAKey, "docker.com/notary3", ts.URL, true)
	defer os.RemoveAll(repo.baseDir)
	testPublishBadMetadata(t, data.CanonicalSnapshotRole, repo, true, true)

	// publish first - publish again should succeed despite corrupt snapshot data (local snapshot signing)
	repo, _ = initializeRepo(t, data.ECDSAKey, "docker.com/notary4", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	testPublishBadMetadata(t, data.CanonicalSnapshotRole, repo, true, true)
}

// If the targets metadata is corrupt or the targets metadata is unreadable,
// we can't publish for the first time, because there is no existing data for.
// us to download. If the repo has already been published, it doesn't matter
// if the metadata is corrupt because we can just redownload if it is.
func TestPublishTargetsCorrupt(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	// do not publish first - publish should fail with corrupt snapshot data
	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary1", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	testPublishBadMetadata(t, data.CanonicalTargetsRole, repo, false, false)

	// publish first - publish again should succeed despite corrupt snapshot data
	repo, _ = initializeRepo(t, data.ECDSAKey, "docker.com/notary2", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	testPublishBadMetadata(t, data.CanonicalTargetsRole, repo, true, true)
}

// If the root metadata is corrupt or the root metadata is unreadable,
// we can't publish for the first time.  If there is already a remote root,
// we just download that and verify (using our trusted certificate trust
// anchors) that it is signed with the same keys, and if so, we just use the
// remote root.
func TestPublishRootCorrupt(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	// do not publish first - publish should fail with corrupt snapshot data
	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary1", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	testPublishBadMetadata(t, data.CanonicalRootRole, repo, false, false)

	// publish first - publish should still succeed if root corrupt since the
	// remote root is signed with the same key.
	repo, _ = initializeRepo(t, data.ECDSAKey, "docker.com/notary2", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	testPublishBadMetadata(t, data.CanonicalRootRole, repo, true, true)
}

// When publishing snapshot, root, or target, if the repo hasn't been published
// before, if the metadata is corrupt, it can't be published.  If it has been
// published already, then the corrupt metadata can just be re-downloaded, so
// publishing is successful.
func testPublishBadMetadata(t *testing.T, roleName string, repo *NotaryRepository,
	publishFirst, succeeds bool) {

	if publishFirst {
		assert.NoError(t, repo.Publish())
	}

	addTarget(t, repo, "v1", "../fixtures/intermediate-ca.crt")

	// readable, but corrupt file
	repo.fileStore.SetMeta(roleName, []byte("this isn't JSON"))
	err := repo.Publish()
	if succeeds {
		assert.NoError(t, err)
	} else {
		assert.Error(t, err)
		assert.IsType(t, &regJson.SyntaxError{}, err)
	}

	// make an unreadable file by creating a directory instead of a file
	path := fmt.Sprintf("%s.%s",
		filepath.Join(repo.baseDir, tufDir, filepath.FromSlash(repo.gun),
			"metadata", roleName), "json")
	os.RemoveAll(path)
	assert.NoError(t, os.Mkdir(path, 0755))
	defer os.RemoveAll(path)

	err = repo.Publish()
	if succeeds {
		assert.NoError(t, err)
	} else {
		assert.Error(t, err)
		assert.IsType(t, &os.PathError{}, err)
	}
}

// If the repo is not initialized, calling repo.Publish() should return ErrRepoNotInitialized
func TestNotInitializedOnPublish(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _, _ := createRepoAndKey(t, data.ECDSAKey, tempBaseDir, gun, ts.URL)

	addTarget(t, repo, "v1", "../fixtures/intermediate-ca.crt")

	err = repo.Publish()
	assert.Error(t, err)
	assert.IsType(t, ErrRepoNotInitialized{}, err)
}

type cannotCreateKeys struct {
	signed.CryptoService
}

func (cs cannotCreateKeys) Create(_, _ string) (data.PublicKey, error) {
	return nil, fmt.Errorf("Oh no I cannot create keys")
}

// If there is an error creating the local keys, no call is made to get a
// remote key.
func TestPublishSnapshotLocalKeysCreatedFirst(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)
	gun := "docker.com/notary"

	requestMade := false
	ts := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { requestMade = true }))
	defer ts.Close()

	repo, err := NewNotaryRepository(
		tempBaseDir, gun, ts.URL, http.DefaultTransport, passphraseRetriever)
	assert.NoError(t, err, "error creating repo: %s", err)

	cs := cryptoservice.NewCryptoService(gun,
		trustmanager.NewKeyMemoryStore(passphraseRetriever))

	rootPubKey, err := cs.Create(data.CanonicalRootRole, data.ECDSAKey)
	assert.NoError(t, err, "error generating root key: %s", err)

	repo.CryptoService = cannotCreateKeys{CryptoService: cs}

	err = repo.Initialize(rootPubKey.ID(), data.CanonicalSnapshotRole)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Oh no I cannot create keys")
	assert.False(t, requestMade)
}

func createKey(t *testing.T, repo *NotaryRepository, role string, x509 bool) data.PublicKey {
	key, err := repo.CryptoService.Create(role, data.ECDSAKey)
	assert.NoError(t, err, "error creating key")

	if x509 {
		start := time.Now().AddDate(0, 0, -1)
		privKey, _, err := repo.CryptoService.GetPrivateKey(key.ID())
		assert.NoError(t, err)
		cert, err := cryptoservice.GenerateCertificate(
			privKey, role, start, start.AddDate(1, 0, 0),
		)
		assert.NoError(t, err)
		return data.NewECDSAx509PublicKey(trustmanager.CertToPEM(cert))
	}
	return key
}

// Publishing delegations works so long as the delegation parent exists by the
// time that delegation addition change is applied.  Most of the tests for
// applying delegation changes in in helpers_test.go (applyTargets tests), so
// this is just a sanity test to make sure Publish calls it correctly and
// no fallback happens.
func TestPublishDelegations(t *testing.T) {
	testPublishDelegations(t, true, false)
	testPublishDelegations(t, false, false)
}

func TestPublishDelegationsX509(t *testing.T) {
	testPublishDelegations(t, true, true)
	testPublishDelegations(t, false, true)
}

func testPublishDelegations(t *testing.T, clearCache, x509Keys bool) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo1, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo1.baseDir)

	delgKey := createKey(t, repo1, "targets/a", x509Keys)

	// This should publish fine, even though targets/a/b is dependent upon
	// targets/a, because these should execute in order
	for _, delgName := range []string{"targets/a", "targets/a/b", "targets/c"} {
		assert.NoError(t,
			repo1.AddDelegation(delgName, []data.PublicKey{delgKey}, []string{""}),
			"error creating delegation")
	}
	assert.Len(t, getChanges(t, repo1), 6, "wrong number of changelist files found")

	var rec *passRoleRecorder
	if clearCache {
		repo1, rec = newRepoToTestRepo(t, repo1, false)
	}

	assert.NoError(t, repo1.Publish())
	assert.Len(t, getChanges(t, repo1), 0, "wrong number of changelist files found")

	if clearCache {
		// when publishing, only the parents of the delegations created need to be signed
		// (and snapshot)
		rec.assertAsked(t, []string{data.CanonicalTargetsRole, "targets/a", data.CanonicalSnapshotRole})
		rec.clear()
	}

	// this should not publish, because targets/z doesn't exist
	assert.NoError(t,
		repo1.AddDelegation("targets/z/y", []data.PublicKey{delgKey}, []string{""}),
		"error creating delegation")
	assert.Len(t, getChanges(t, repo1), 2, "wrong number of changelist files found")
	assert.Error(t, repo1.Publish())
	assert.Len(t, getChanges(t, repo1), 2, "wrong number of changelist files found")

	if clearCache {
		rec.assertAsked(t, nil)
	}

	// use another repo to check metadata
	repo2, _ := newRepoToTestRepo(t, repo1, false)
	defer os.RemoveAll(repo2.baseDir)

	// pull
	_, err := repo2.ListTargets()
	assert.NoError(t, err, "unable to pull repo")

	for _, repo := range []*NotaryRepository{repo1, repo2} {
		// targets should have delegations targets/a and targets/c
		targets := repo.tufRepo.Targets[data.CanonicalTargetsRole]
		assert.Len(t, targets.Signed.Delegations.Roles, 2)
		assert.Len(t, targets.Signed.Delegations.Keys, 1)

		_, ok := targets.Signed.Delegations.Keys[delgKey.ID()]
		assert.True(t, ok)

		foundRoleNames := make(map[string]bool)
		for _, r := range targets.Signed.Delegations.Roles {
			foundRoleNames[r.Name] = true
		}
		assert.True(t, foundRoleNames["targets/a"])
		assert.True(t, foundRoleNames["targets/c"])

		// targets/a should have delegation targets/a/b only
		a := repo.tufRepo.Targets["targets/a"]
		assert.Len(t, a.Signed.Delegations.Roles, 1)
		assert.Len(t, a.Signed.Delegations.Keys, 1)

		_, ok = a.Signed.Delegations.Keys[delgKey.ID()]
		assert.True(t, ok)

		assert.Equal(t, "targets/a/b", a.Signed.Delegations.Roles[0].Name)
	}
}

// If a changelist specifies a particular role to push targets to, and there
// is no such role, publish will try to publish to its parent.  If the parent
// doesn't work, it falls back on its parent, and so forth, and eventually
// falls back on publishing to "target".  This *only* falls back if the role
// doesn't exist, not if the user doesn't have a key.  (different test)
func TestPublishTargetsDelgationScopeFallback(t *testing.T) {
	testPublishTargetsDelgationScopeFallback(t, true)
	testPublishTargetsDelgationScopeFallback(t, false)
}

func testPublishTargetsDelgationScopeFallback(t *testing.T, clearCache bool) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	var rec *passRoleRecorder
	if clearCache {
		repo, rec = newRepoToTestRepo(t, repo, false)
	}

	assertPublishToRolesSucceeds(t, repo, []string{"targets/a/b", "targets/b/c"},
		[]string{data.CanonicalTargetsRole})

	if clearCache {
		rec.assertAsked(t, []string{data.CanonicalTargetsRole, data.CanonicalSnapshotRole})
		rec.assertCreated(t, nil)
	}
}

// If a changelist specifies a particular role to push targets to, and there
// is a role but no key, publish not fall back and just fail.
func TestPublishTargetsDelgationScopeNoFallbackIfNoKeys(t *testing.T) {
	testPublishTargetsDelgationScopeNoFallbackIfNoKeys(t, true)
	testPublishTargetsDelgationScopeNoFallbackIfNoKeys(t, false)
}

func testPublishTargetsDelgationScopeNoFallbackIfNoKeys(t *testing.T, clearCache bool) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// generate a key that isn't in the cryptoservice, so we can't sign this
	// one
	aPrivKey, err := trustmanager.GenerateECDSAKey(rand.Reader)
	assert.NoError(t, err, "error generating key that is not in our cryptoservice")
	aPubKey := data.PublicKeyFromPrivate(aPrivKey)

	var rec *passRoleRecorder
	if clearCache {
		repo, rec = newRepoToTestRepo(t, repo, false)
	}

	// ensure that the role exists
	assert.NoError(t, repo.AddDelegation("targets/a", []data.PublicKey{aPubKey}, []string{""}))
	assert.NoError(t, repo.Publish())

	if clearCache {
		rec.assertAsked(t, []string{data.CanonicalTargetsRole, data.CanonicalSnapshotRole})
		rec.clear()
	}

	// add a target to targets/a/b - no role b, so it falls back on a, which
	// exists but there is no signing key for
	addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt", "targets/a/b")
	assert.Len(t, getChanges(t, repo), 1, "wrong number of changelist files found")

	// Now Publish should fail
	assert.Error(t, repo.Publish())
	assert.Len(t, getChanges(t, repo), 1, "wrong number of changelist files found")
	if clearCache {
		rec.assertAsked(t, nil)
		rec.clear()
	}

	targets, err := repo.ListTargets("targets", "targets/a", "targets/a/b")
	assert.NoError(t, err)
	assert.Empty(t, targets)
}

// If a changelist specifies a particular role to push targets to, and such
// a role and the keys are present, publish will write to that role only, and
// not its parents.  This tests the case where the local machine knows about
// all the roles (in fact, the role creations will be applied before the
// targets)
func TestPublishTargetsDelgationSuccessLocallyHasRoles(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	for _, delgName := range []string{"targets/a", "targets/a/b"} {
		delgKey := createKey(t, repo, delgName, false)
		assert.NoError(t,
			repo.AddDelegation(delgName, []data.PublicKey{delgKey}, []string{""}),
			"error creating delegation")
	}

	// just always check signing now, we've already established we can publish
	// delgations with and without the metadata and key cache
	var rec *passRoleRecorder
	repo, rec = newRepoToTestRepo(t, repo, false)

	assertPublishToRolesSucceeds(t, repo, []string{"targets/a/b"},
		[]string{"targets/a/b"})

	// first time publishing, so everything gets signed
	rec.assertAsked(t, []string{data.CanonicalTargetsRole, "targets/a", "targets/a/b",
		data.CanonicalSnapshotRole})
}

// If a changelist specifies a particular role to push targets to, and the role
// is present, publish will write to that role only.  The targets keys are not
// needed.
func TestPublishTargetsDelgationNoTargetsKeyNeeded(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	for _, delgName := range []string{"targets/a", "targets/a/b"} {
		delgKey := createKey(t, repo, delgName, false)
		assert.NoError(t,
			repo.AddDelegation(delgName, []data.PublicKey{delgKey}, []string{""}),
			"error creating delegation")
	}

	// just always check signing now, we've already established we can publish
	// delgations with and without the metadata and key cache
	var rec *passRoleRecorder
	repo, rec = newRepoToTestRepo(t, repo, false)

	assert.NoError(t, repo.Publish())
	// first time publishing, so all delegation parents get signed
	rec.assertAsked(t, []string{data.CanonicalTargetsRole, "targets/a", data.CanonicalSnapshotRole})
	rec.clear()

	// remove targets key - it is not even needed
	targetsKeys := repo.CryptoService.ListKeys(data.CanonicalTargetsRole)
	assert.Len(t, targetsKeys, 1)
	assert.NoError(t, repo.CryptoService.RemoveKey(targetsKeys[0]))

	assertPublishToRolesSucceeds(t, repo, []string{"targets/a/b"},
		[]string{"targets/a/b"})

	// only the target delegation gets signed - snapshot key has already been cached
	rec.assertAsked(t, []string{"targets/a/b"})
}

// If a changelist specifies a particular role to push targets to, and is such
// a role and the keys are present, publish will write to that role only, and
// not its parents.  Tests:
// - case where the local doesn't know about all the roles, and has to download
//   them before publish.
// - owner of a repo may not have the delegated keys, so can't sign a delegated
//   role
func TestPublishTargetsDelgationSuccessNeedsToDownloadRoles(t *testing.T) {
	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	// this is the original repo - it owns the root/targets keys and creates
	// the delegation to which it doesn't have the key (so server snapshot
	// signing would be required)
	ownerRepo, _ := initializeRepo(t, data.ECDSAKey, gun, ts.URL, true)
	defer os.RemoveAll(ownerRepo.baseDir)

	// this is a user, or otherwise a repo that only has access to the delegation
	// key so it can publish targets to the delegated role
	delgRepo, _ := newRepoToTestRepo(t, ownerRepo, true)
	defer os.RemoveAll(delgRepo.baseDir)

	// create a key on the owner repo
	aKey, err := ownerRepo.CryptoService.Create("targets/a", data.ECDSAKey)
	assert.NoError(t, err, "error creating delegation key")

	// create a key on the delegated repo
	bKey, err := delgRepo.CryptoService.Create("targets/a/b", data.ECDSAKey)
	assert.NoError(t, err, "error creating delegation key")

	// clear metadata and unencrypted private key cache
	var ownerRec, delgRec *passRoleRecorder
	ownerRepo, ownerRec = newRepoToTestRepo(t, ownerRepo, false)
	delgRepo, delgRec = newRepoToTestRepo(t, delgRepo, false)

	// owner creates delegations, adds the delegated key to them, and publishes them
	assert.NoError(t,
		ownerRepo.AddDelegation("targets/a", []data.PublicKey{aKey}, []string{""}),
		"error creating delegation")
	assert.NoError(t,
		ownerRepo.AddDelegation("targets/a/b", []data.PublicKey{bKey}, []string{""}),
		"error creating delegation")

	assert.NoError(t, ownerRepo.Publish())
	// delegation parents all get signed
	ownerRec.assertAsked(t, []string{data.CanonicalTargetsRole, "targets/a"})

	// delegated repo now publishes to delegated roles, but it will need
	// to download those roles first, since it doesn't know about them
	assertPublishToRolesSucceeds(t, delgRepo, []string{"targets/a/b"},
		[]string{"targets/a/b"})
	delgRec.assertAsked(t, []string{"targets/a/b"})
}

// Ensure that two clients can publish delegations with two different keys and
// the changes will not clobber each other.
func TestPublishTargetsDelgationFromTwoRepos(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	// this happens to be the client that creates the repo, but can also
	// write a delegation
	repo1, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, true)
	defer os.RemoveAll(repo1.baseDir)

	// this is the second writable repo
	repo2, _ := newRepoToTestRepo(t, repo1, true)
	defer os.RemoveAll(repo2.baseDir)

	// create keys for each repo
	key1, err := repo1.CryptoService.Create("targets/a", data.ECDSAKey)
	assert.NoError(t, err, "error creating delegation key")

	// create a key on the delegated repo
	key2, err := repo2.CryptoService.Create("targets/a", data.ECDSAKey)
	assert.NoError(t, err, "error creating delegation key")

	// delegation includes both keys
	assert.NoError(t,
		repo1.AddDelegation("targets/a", []data.PublicKey{key1, key2}, []string{""}),
		"error creating delegation")

	assert.NoError(t, repo1.Publish())

	// clear metadata and unencrypted private key cache
	var rec1, rec2 *passRoleRecorder
	repo1, rec1 = newRepoToTestRepo(t, repo1, false)
	repo2, rec2 = newRepoToTestRepo(t, repo2, false)

	// both repos add targets and publish
	addTarget(t, repo1, "first", "../fixtures/root-ca.crt", "targets/a")
	assert.NoError(t, repo1.Publish())
	rec1.assertAsked(t, []string{"targets/a"})
	rec1.clear()

	addTarget(t, repo2, "second", "../fixtures/root-ca.crt", "targets/a")
	assert.NoError(t, repo2.Publish())
	rec2.assertAsked(t, []string{"targets/a"})
	rec2.clear()

	// first repo can publish again
	addTarget(t, repo1, "third", "../fixtures/root-ca.crt", "targets/a")
	assert.NoError(t, repo1.Publish())
	// key has been cached now
	rec1.assertAsked(t, nil)
	rec1.clear()

	// both repos should be able to see all targets
	for _, repo := range []*NotaryRepository{repo1, repo2} {
		targets, err := repo.ListTargets()
		assert.NoError(t, err)
		assert.Len(t, targets, 3)

		found := make(map[string]bool)
		for _, t := range targets {
			found[t.Name] = true
		}

		for _, targetName := range []string{"first", "second", "third"} {
			_, ok := found[targetName]
			assert.True(t, ok)
		}
	}
}

// A client who could publish before can no longer publish once the owner
// removes their delegation key from the delegation role.
func TestPublishRemoveDelgationKeyFromDelegationRole(t *testing.T) {
	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	// this is the original repo - it owns the root/targets keys and creates
	// the delegation to which it doesn't have the key (so server snapshot
	// signing would be required)
	ownerRepo, _ := initializeRepo(t, data.ECDSAKey, gun, ts.URL, true)
	defer os.RemoveAll(ownerRepo.baseDir)

	// this is a user, or otherwise a repo that only has access to the delegation
	// key so it can publish targets to the delegated role
	delgRepo, _ := newRepoToTestRepo(t, ownerRepo, true)
	defer os.RemoveAll(delgRepo.baseDir)

	// create a key on the delegated repo
	aKey, err := delgRepo.CryptoService.Create("targets/a", data.ECDSAKey)
	assert.NoError(t, err, "error creating delegation key")

	// owner creates delegation, adds the delegated key to it, and publishes it
	assert.NoError(t,
		ownerRepo.AddDelegation("targets/a", []data.PublicKey{aKey}, []string{""}),
		"error creating delegation")
	assert.NoError(t, ownerRepo.Publish())

	// delegated repo can now publish to delegated role
	addTarget(t, delgRepo, "v1", "../fixtures/root-ca.crt", "targets/a")
	assert.NoError(t, delgRepo.Publish())

	// owner revokes delegation
	// note there is no removekeyfromdelegation yet, so here's a hack to do so
	newKey, err := ownerRepo.CryptoService.Create("targets/a", data.ECDSAKey)
	assert.NoError(t, err)
	tdJSON, err := json.Marshal(&changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      data.KeyList([]data.PublicKey{newKey}),
		RemoveKeys:   []string{aKey.ID()},
	})
	assert.NoError(t, err)

	cl, err := changelist.NewFileChangelist(filepath.Join(ownerRepo.tufRepoPath, "changelist"))
	assert.NoError(t, cl.Add(changelist.NewTufChange(
		changelist.ActionUpdate,
		"targets/a",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)))
	cl.Close()
	assert.NoError(t, ownerRepo.Publish())

	// delegated repo can now no longer publish to delegated role
	addTarget(t, delgRepo, "v2", "../fixtures/root-ca.crt", "targets/a")
	assert.Error(t, delgRepo.Publish())
}

// A client who could publish before can no longer publish once the owner
// deletes the delegation
func TestPublishRemoveDelgation(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	// this is the original repo - it owns the root/targets keys and creates
	// the delegation to which it doesn't have the key (so server snapshot
	// signing would be required)
	ownerRepo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, true)
	defer os.RemoveAll(ownerRepo.baseDir)

	// this is a user, or otherwise a repo that only has access to the delegation
	// key so it can publish targets to the delegated role
	delgRepo, _ := newRepoToTestRepo(t, ownerRepo, true)
	defer os.RemoveAll(delgRepo.baseDir)

	// create a key on the delegated repo
	aKey, err := delgRepo.CryptoService.Create("targets/a", data.ECDSAKey)
	assert.NoError(t, err, "error creating delegation key")

	// owner creates delegation, adds the delegated key to it, and publishes it
	assert.NoError(t,
		ownerRepo.AddDelegation("targets/a", []data.PublicKey{aKey}, []string{""}),
		"error creating delegation")
	assert.NoError(t, ownerRepo.Publish())

	// delegated repo can now publish to delegated role
	addTarget(t, delgRepo, "v1", "../fixtures/root-ca.crt", "targets/a")
	assert.NoError(t, delgRepo.Publish())

	// owner removes delegation
	aKeyCanonicalID, err := utils.CanonicalKeyID(aKey)
	assert.NoError(t, err)
	assert.NoError(t, ownerRepo.RemoveDelegationKeys("targets/a", []string{aKeyCanonicalID}))
	assert.NoError(t, ownerRepo.Publish())

	// delegated repo can now no longer publish to delegated role
	addTarget(t, delgRepo, "v2", "../fixtures/root-ca.crt", "targets/a")
	assert.Error(t, delgRepo.Publish())
}

// If the delegation data is corrupt or unreadable, it doesn't matter because
// all the delegation information is just re-downloaded.  When bootstrapping
// the repository from disk, we just don't load the data from disk because
// there should not be anything there yet.
func TestPublishSucceedsDespiteDelegationCorrupt(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	delgKey, err := repo.CryptoService.Create("targets/a", data.ECDSAKey)
	assert.NoError(t, err, "error creating delegation key")

	assert.NoError(t,
		repo.AddDelegation("targets/a", []data.PublicKey{delgKey}, []string{""}),
		"error creating delegation")

	testPublishBadMetadata(t, "targets/a", repo, false, true)

	// publish again, now that it has already been published, and again there
	// is no error.
	testPublishBadMetadata(t, "targets/a", repo, true, true)
}

// Rotate invalid roles, or attempt to delegate target signing to the server
func TestRotateKeyInvalidRole(t *testing.T) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// the equivalent of: (root, true), (root, false), (timestamp, true),
	// (timestamp, false), (targets, true)
	for _, role := range data.BaseRoles {
		if role == data.CanonicalSnapshotRole {
			continue
		}
		for _, serverManagesKey := range []bool{true, false} {
			if role == data.CanonicalTargetsRole && !serverManagesKey {
				continue
			}
			err := repo.RotateKey(role, serverManagesKey)
			assert.Error(t, err,
				"Rotating a %s key with server-managing the key as %v should fail",
				role, serverManagesKey)
		}
	}
}

// Rotates the keys.  After the rotation, downloading the latest metadata
// and assert that the keys have changed
func assertRotationSuccessful(t *testing.T, repo1 *NotaryRepository,
	keysToRotate map[string]bool, alreadyPublished bool) {
	// Create 2 new repos:  1 will download repo data before the publish,
	// and one only downloads after the publish. This reflects a client
	// that has some previous trust data (but is not the publisher), and a
	// completely new client being able to read the rotated trust data.
	repo2, _ := newRepoToTestRepo(t, repo1, true)
	defer os.RemoveAll(repo2.baseDir)

	repos := []*NotaryRepository{repo1, repo2}

	if alreadyPublished {
		repo3, _ := newRepoToTestRepo(t, repo1, true)
		defer os.RemoveAll(repo2.baseDir)

		// force a pull on repo3
		_, err := repo3.GetTargetByName("latest")
		assert.NoError(t, err)

		repos = append(repos, repo3)
	}

	oldKeyIDs := make(map[string][]string)
	for role := range keysToRotate {
		keyIDs := repo1.tufRepo.Root.Signed.Roles[role].KeyIDs
		oldKeyIDs[role] = keyIDs
	}

	// Do rotation
	for role, serverManaged := range keysToRotate {
		assert.NoError(t, repo1.RotateKey(role, serverManaged))
	}

	// Publish
	err := repo1.Publish()
	assert.NoError(t, err)

	// Download data from remote and check that keys have changed
	for _, repo := range repos {
		_, err := repo.GetTargetByName("latest") // force a pull
		assert.NoError(t, err)

		for role, isRemoteKey := range keysToRotate {
			keyIDs := repo.tufRepo.Root.Signed.Roles[role].KeyIDs
			assert.Len(t, keyIDs, 1)

			// the new key is not the same as any of the old keys, and the
			// old keys have been removed not just from the TUF file, but
			// from the cryptoservice
			for _, oldKeyID := range oldKeyIDs[role] {
				assert.NotEqual(t, oldKeyID, keyIDs[0])
				_, _, err := repo.CryptoService.GetPrivateKey(oldKeyID)
				assert.Error(t, err)
			}

			// On the old repo, the new key is present in the cryptoservice, or
			// not present if remote.  On the new repo, no keys are ever in the
			// cryptoservice
			key, _, err := repo.CryptoService.GetPrivateKey(keyIDs[0])
			if repo != repo1 || isRemoteKey {
				assert.Error(t, err)
				assert.Nil(t, key)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, key)
			}
		}

		// Confirm changelist dir empty (on repo1, it should be empty after
		// after publishing changes, on repo2, there should never have been
		// any changelists)
		changes := getChanges(t, repo)
		assert.Len(t, changes, 0, "wrong number of changelist files found")
	}
}

// Initialize repo to have the server sign snapshots (remote snapshot key)
// Without downloading a server-signed snapshot file, rotate keys so that
//    snapshots are locally signed (local snapshot key)
// Assert that we can publish.
func TestRotateBeforePublishFromRemoteKeyToLocalKey(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, true)
	defer os.RemoveAll(repo.baseDir)

	// Adding a target will allow us to confirm the repository is still valid
	// after rotating the keys.
	addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt")
	assertRotationSuccessful(t, repo, map[string]bool{
		data.CanonicalTargetsRole:  false,
		data.CanonicalSnapshotRole: false}, false)
}

// Initialize a repo, locally signed snapshots
// Publish some content (so that the server has a root.json), and download root.json
// Rotate keys
// Download the latest metadata and assert that the keys have changed.
func TestRotateKeyAfterPublishNoServerManagementChange(t *testing.T) {
	// rotate a single target key
	testRotateKeySuccess(t, false, map[string]bool{data.CanonicalTargetsRole: false})
	testRotateKeySuccess(t, false, map[string]bool{data.CanonicalSnapshotRole: false})
	// rotate two at once before publishing
	testRotateKeySuccess(t, false, map[string]bool{
		data.CanonicalSnapshotRole: false,
		data.CanonicalTargetsRole:  false})
}

// Tests rotating keys when there's a change from locally managed keys to
// remotely managed keys and vice versa
// Before rotating, publish some content (so that the server has a root.json),
// and download root.json
func TestRotateKeyAfterPublishServerManagementChange(t *testing.T) {
	// delegate snapshot key management to the server
	testRotateKeySuccess(t, false, map[string]bool{
		data.CanonicalSnapshotRole: true,
		data.CanonicalTargetsRole:  false,
	})
	// reclaim snapshot key management from the server
	testRotateKeySuccess(t, true, map[string]bool{
		data.CanonicalSnapshotRole: false,
		data.CanonicalTargetsRole:  false,
	})
}

func testRotateKeySuccess(t *testing.T, serverManagesSnapshotInit bool,
	keysToRotate map[string]bool) {

	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL,
		serverManagesSnapshotInit)
	defer os.RemoveAll(repo.baseDir)

	// Adding a target will allow us to confirm the repository is still valid after
	// rotating the keys.
	addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt")

	// Publish
	assert.NoError(t, repo.Publish())

	// Get root.json and capture targets + snapshot key IDs
	repo.GetTargetByName("latest") // force a pull
	assertRotationSuccessful(t, repo, keysToRotate, true)

	var keysToExpectCreated []string
	for role, serverManaged := range keysToRotate {
		if !serverManaged {
			keysToExpectCreated = append(keysToExpectCreated, role)
		}
	}
}

// If there is no local cache, notary operations return the remote error code
func TestRemoteServerUnavailableNoLocalCache(t *testing.T) {
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)
	defer os.RemoveAll(tempBaseDir)

	ts := errorTestServer(t, 500)
	defer ts.Close()

	repo, err := NewNotaryRepository(tempBaseDir, "docker.com/notary",
		ts.URL, http.DefaultTransport, passphraseRetriever)
	assert.NoError(t, err, "error creating repo: %s", err)

	_, err = repo.ListTargets(data.CanonicalTargetsRole)
	assert.Error(t, err)
	assert.IsType(t, store.ErrServerUnavailable{}, err)

	_, err = repo.GetTargetByName("targetName")
	assert.Error(t, err)
	assert.IsType(t, store.ErrServerUnavailable{}, err)

	err = repo.Publish()
	assert.Error(t, err)
	assert.IsType(t, store.ErrServerUnavailable{}, err)
}

// AddDelegation creates a valid changefile (rejects invalid delegation names,
// but does not check the delegation hierarchy).  When applied, the change adds
// a new delegation role with the correct keys.
func TestAddDelegationChangefileValid(t *testing.T) {
	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	targetKeyIds := repo.CryptoService.ListKeys(data.CanonicalTargetsRole)
	assert.NotEmpty(t, targetKeyIds)
	targetPubKey := repo.CryptoService.GetKey(targetKeyIds[0])
	assert.NotNil(t, targetPubKey)

	err := repo.AddDelegation("root", []data.PublicKey{targetPubKey}, []string{""})
	assert.Error(t, err)
	assert.IsType(t, data.ErrInvalidRole{}, err)
	assert.Empty(t, getChanges(t, repo))

	// to show that adding does not care about the hierarchy
	err = repo.AddDelegation("targets/a/b/c", []data.PublicKey{targetPubKey}, []string{""})
	assert.NoError(t, err)

	// ensure that the changefiles is correct
	changes := getChanges(t, repo)
	assert.Len(t, changes, 2)
	assert.Equal(t, changelist.ActionCreate, changes[0].Action())
	assert.Equal(t, "targets/a/b/c", changes[0].Scope())
	assert.Equal(t, changelist.TypeTargetsDelegation, changes[0].Type())
	assert.Equal(t, changelist.ActionCreate, changes[1].Action())
	assert.Equal(t, "targets/a/b/c", changes[1].Scope())
	assert.Equal(t, changelist.TypeTargetsDelegation, changes[1].Type())
	assert.Equal(t, "", changes[1].Path())
	assert.NotEmpty(t, changes[0].Content())
}

// The changefile produced by AddDelegation, when applied, actually adds
// the delegation to the repo (assuming the delegation hierarchy is correct -
// tests for change application validation are in helpers_test.go)
func TestAddDelegationChangefileApplicable(t *testing.T) {
	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	targetKeyIds := repo.CryptoService.ListKeys(data.CanonicalTargetsRole)
	assert.NotEmpty(t, targetKeyIds)
	targetPubKey := repo.CryptoService.GetKey(targetKeyIds[0])
	assert.NotNil(t, targetPubKey)

	// this hierarchy has to be right to be applied
	err := repo.AddDelegation("targets/a", []data.PublicKey{targetPubKey}, []string{""})
	assert.NoError(t, err)
	changes := getChanges(t, repo)
	assert.Len(t, changes, 2)

	// ensure that it can be applied correctly
	err = applyTargetsChange(repo.tufRepo, changes[0])
	assert.NoError(t, err)

	targetRole := repo.tufRepo.Targets[data.CanonicalTargetsRole]
	assert.Len(t, targetRole.Signed.Delegations.Roles, 1)
	assert.Len(t, targetRole.Signed.Delegations.Keys, 1)

	_, ok := targetRole.Signed.Delegations.Keys[targetPubKey.ID()]
	assert.True(t, ok)

	newDelegationRole := targetRole.Signed.Delegations.Roles[0]
	assert.Len(t, newDelegationRole.KeyIDs, 1)
	assert.Equal(t, targetPubKey.ID(), newDelegationRole.KeyIDs[0])
	assert.Equal(t, "targets/a", newDelegationRole.Name)
}

// TestAddDelegationErrorWritingChanges expects errors writing a change to file
// to be propagated.
func TestAddDelegationErrorWritingChanges(t *testing.T) {
	testErrorWritingChangefiles(t, func(repo *NotaryRepository) error {
		targetKeyIds := repo.CryptoService.ListKeys(data.CanonicalTargetsRole)
		assert.NotEmpty(t, targetKeyIds)
		targetPubKey := repo.CryptoService.GetKey(targetKeyIds[0])
		assert.NotNil(t, targetPubKey)

		return repo.AddDelegation("targets/a", []data.PublicKey{targetPubKey}, []string{""})
	})
}

// RemoveDelegation rejects attempts to remove invalidly-named delegations,
// but otherwise does not validate the name of the delegation to remove.  This
// test ensures that the changefile generated by RemoveDelegation is correct.
func TestRemoveDelegationChangefileValid(t *testing.T) {
	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	rootPubKey := repo.CryptoService.GetKey(rootKeyID)
	assert.NotNil(t, rootPubKey)

	err := repo.RemoveDelegationKeys("root", []string{rootKeyID})
	assert.Error(t, err)
	assert.IsType(t, data.ErrInvalidRole{}, err)
	assert.Empty(t, getChanges(t, repo))

	// to demonstrate that so long as the delegation name is valid, the
	// existence of the delegation doesn't matter
	assert.NoError(t, repo.RemoveDelegationKeys("targets/a/b/c", []string{rootKeyID}))

	// ensure that the changefile is correct
	changes := getChanges(t, repo)
	assert.Len(t, changes, 1)
	assert.Equal(t, changelist.ActionUpdate, changes[0].Action())
	assert.Equal(t, "targets/a/b/c", changes[0].Scope())
	assert.Equal(t, changelist.TypeTargetsDelegation, changes[0].Type())
	assert.Equal(t, "", changes[0].Path())
}

// The changefile produced by RemoveDelegationKeys, when applied, actually removes
// the delegation from the repo (assuming the repo exists - tests for
// change application validation are in helpers_test.go)
func TestRemoveDelegationChangefileApplicable(t *testing.T) {
	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	rootPubKey := repo.CryptoService.GetKey(rootKeyID)
	assert.NotNil(t, rootPubKey)

	// add a delegation first so it can be removed
	assert.NoError(t, repo.AddDelegation("targets/a", []data.PublicKey{rootPubKey}, []string{""}))
	changes := getChanges(t, repo)
	assert.Len(t, changes, 2)
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[0]))
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[1]))

	targetRole := repo.tufRepo.Targets[data.CanonicalTargetsRole]
	assert.Len(t, targetRole.Signed.Delegations.Roles, 1)
	assert.Len(t, targetRole.Signed.Delegations.Keys, 1)

	// now remove it
	rootKeyCanonicalID, err := utils.CanonicalKeyID(rootPubKey)
	assert.NoError(t, err)
	assert.NoError(t, repo.RemoveDelegationKeys("targets/a", []string{rootKeyCanonicalID}))
	changes = getChanges(t, repo)
	assert.Len(t, changes, 3)
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[2]))

	targetRole = repo.tufRepo.Targets[data.CanonicalTargetsRole]
	assert.Empty(t, targetRole.Signed.Delegations.Roles)
	assert.Empty(t, targetRole.Signed.Delegations.Keys)
}

// The changefile with the ClearAllPaths key set, when applied, actually removes
// all paths from the specified delegation in the repo (assuming the repo and delegation exist)
func TestClearAllPathsDelegationChangefileApplicable(t *testing.T) {
	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	rootPubKey := repo.CryptoService.GetKey(rootKeyID)
	assert.NotNil(t, rootPubKey)

	// add a delegation first so it can be removed
	assert.NoError(t, repo.AddDelegation("targets/a", []data.PublicKey{rootPubKey}, []string{"abc,123,xyz,path"}))
	changes := getChanges(t, repo)
	assert.Len(t, changes, 2)
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[0]))
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[1]))

	// now clear paths it
	assert.NoError(t, repo.ClearDelegationPaths("targets/a"))
	changes = getChanges(t, repo)
	assert.Len(t, changes, 3)
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[2]))

	delgRoles := repo.tufRepo.Targets[data.CanonicalTargetsRole].Signed.Delegations.Roles
	assert.Len(t, delgRoles, 1)
	assert.Len(t, delgRoles[0].Paths, 0)
}

// TestFullAddDelegationChangefileApplicable generates a single changelist with AddKeys and AddPaths set,
// (in the old style of AddDelegation) and tests that all of its changes are reflected on publish
func TestFullAddDelegationChangefileApplicable(t *testing.T) {
	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	rootPubKey := repo.CryptoService.GetKey(rootKeyID)
	assert.NotNil(t, rootPubKey)

	key2, err := repo.CryptoService.Create("user", data.ECDSAKey)
	assert.NoError(t, err)

	delegationName := "targets/a"

	// manually create the changelist object to load multiple keys
	tdJSON, err := json.Marshal(&changelist.TufDelegation{
		NewThreshold: notary.MinThreshold,
		AddKeys:      data.KeyList([]data.PublicKey{rootPubKey, key2}),
		AddPaths:     []string{"abc", "123", "xyz"},
	})
	change := newCreateDelegationChange(delegationName, tdJSON)
	cl, err := changelist.NewFileChangelist(filepath.Join(repo.tufRepoPath, "changelist"))
	addChange(cl, change, delegationName)

	changes := getChanges(t, repo)
	assert.Len(t, changes, 1)
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[0]))

	delgRoles := repo.tufRepo.Targets[data.CanonicalTargetsRole].Signed.Delegations.Roles
	assert.Len(t, delgRoles, 1)
	assert.Len(t, delgRoles[0].Paths, 3)
	assert.Len(t, delgRoles[0].KeyIDs, 2)
	assert.Equal(t, delgRoles[0].Name, delegationName)
}

// TestFullRemoveDelegationChangefileApplicable generates a single changelist with RemoveKeys and RemovePaths set,
// (in the old style of RemoveDelegation) and tests that all of its changes are reflected on publish
func TestFullRemoveDelegationChangefileApplicable(t *testing.T) {
	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)
	rootPubKey := repo.CryptoService.GetKey(rootKeyID)
	assert.NotNil(t, rootPubKey)

	key2, err := repo.CryptoService.Create("user", data.ECDSAKey)
	assert.NoError(t, err)
	key2CanonicalID, err := utils.CanonicalKeyID(key2)
	assert.NoError(t, err)

	delegationName := "targets/a"

	assert.NoError(t, repo.AddDelegation(delegationName, []data.PublicKey{rootPubKey, key2}, []string{"abc", "123"}))
	changes := getChanges(t, repo)
	assert.Len(t, changes, 2)
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[0]))
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[1]))

	targetRole := repo.tufRepo.Targets[data.CanonicalTargetsRole]
	assert.Len(t, targetRole.Signed.Delegations.Roles, 1)
	assert.Len(t, targetRole.Signed.Delegations.Keys, 2)

	// manually create the changelist object to load multiple keys
	tdJSON, err := json.Marshal(&changelist.TufDelegation{
		RemoveKeys:  []string{key2CanonicalID},
		RemovePaths: []string{"abc", "123"},
	})
	change := newUpdateDelegationChange(delegationName, tdJSON)
	cl, err := changelist.NewFileChangelist(filepath.Join(repo.tufRepoPath, "changelist"))
	addChange(cl, change, delegationName)

	changes = getChanges(t, repo)
	assert.Len(t, changes, 3)
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[2]))

	delgRoles := repo.tufRepo.Targets[data.CanonicalTargetsRole].Signed.Delegations.Roles
	assert.Len(t, delgRoles, 1)
	assert.Len(t, delgRoles[0].Paths, 0)
	assert.Len(t, delgRoles[0].KeyIDs, 1)
}

// TestRemoveDelegationErrorWritingChanges expects errors writing a change to
// file to be propagated.
func TestRemoveDelegationErrorWritingChanges(t *testing.T) {
	testErrorWritingChangefiles(t, func(repo *NotaryRepository) error {
		return repo.RemoveDelegationKeysAndPaths("targets/a", []string{""}, []string{})
	})
}

// TestBootstrapClientBadURL checks that bootstrapClient correctly
// returns an error when the URL is valid but does not point to
// a TUF server
func TestBootstrapClientBadURL(t *testing.T) {
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)
	repo, err := NewNotaryRepository(
		tempBaseDir,
		"testGun",
		"http://localhost:9998",
		http.DefaultTransport,
		passphraseRetriever,
	)
	assert.NoError(t, err, "error creating repo: %s", err)

	c, err := repo.bootstrapClient(false)
	assert.Nil(t, c)
	assert.Error(t, err)

	c, err2 := repo.bootstrapClient(true)
	assert.Nil(t, c)
	assert.Error(t, err2)

	// same error should be returned because we don't have local data
	// and are requesting remote root regardless of checkInitialized
	// value
	assert.EqualError(t, err, err2.Error())
}

// TestBootstrapClientInvalidURL checks that bootstrapClient correctly
// returns an error when the URL is valid but does not point to
// a TUF server
func TestBootstrapClientInvalidURL(t *testing.T) {
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)
	repo, err := NewNotaryRepository(
		tempBaseDir,
		"testGun",
		"#!*)&!)#*^%!#)%^!#",
		http.DefaultTransport,
		passphraseRetriever,
	)
	assert.NoError(t, err, "error creating repo: %s", err)

	c, err := repo.bootstrapClient(false)
	assert.Nil(t, c)
	assert.Error(t, err)

	c, err2 := repo.bootstrapClient(true)
	assert.Nil(t, c)
	assert.Error(t, err2)

	// same error should be returned because we don't have local data
	// and are requesting remote root regardless of checkInitialized
	// value
	assert.EqualError(t, err, err2.Error())
}

func TestPublishTargetsDelgationCanUseUserKeyWithArbitraryRole(t *testing.T) {
	testPublishTargetsDelgationCanUseUserKeyWithArbitraryRole(t, false)
	testPublishTargetsDelgationCanUseUserKeyWithArbitraryRole(t, true)
}

func testPublishTargetsDelgationCanUseUserKeyWithArbitraryRole(t *testing.T, x509 bool) {
	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	// this is the original repo - it owns the root/targets keys and creates
	// the delegation to which it doesn't have the key (so server snapshot
	// signing would be required)
	ownerRepo, _ := initializeRepo(t, data.ECDSAKey, gun, ts.URL, true)
	defer os.RemoveAll(ownerRepo.baseDir)

	// this is a user, or otherwise a repo that only has access to the delegation
	// key so it can publish targets to the delegated role
	delgRepo, _ := newRepoToTestRepo(t, ownerRepo, true)
	defer os.RemoveAll(delgRepo.baseDir)

	// create a key on the owner repo
	aKey := createKey(t, ownerRepo, "user", x509)
	aKeyID, err := utils.CanonicalKeyID(aKey)
	assert.NoError(t, err)
	// move this to the tuf_keys directory without any GUN, and ensure that we
	// can sign with it
	assert.NoError(t, os.Rename(
		filepath.Join(ownerRepo.baseDir, "private/tuf_keys", gun, aKeyID+".key"),
		filepath.Join(ownerRepo.baseDir, "private/tuf_keys", aKeyID+".key")))

	// create a key on the delegated repo
	bKey := createKey(t, delgRepo, "notARealRoleName", x509)
	bKeyID, err := utils.CanonicalKeyID(bKey)
	assert.NoError(t, err)
	assert.NoError(t, os.Rename(
		filepath.Join(delgRepo.baseDir, "private/tuf_keys", gun, bKeyID+".key"),
		filepath.Join(delgRepo.baseDir, "private/tuf_keys", bKeyID+".key")))

	// clear metadata and unencrypted private key cache
	var ownerRec, delgRec *passRoleRecorder
	ownerRepo, ownerRec = newRepoToTestRepo(t, ownerRepo, false)
	delgRepo, delgRec = newRepoToTestRepo(t, delgRepo, false)

	// owner creates delegations, adds the delegated key to them, and publishes them
	assert.NoError(t,
		ownerRepo.AddDelegation("targets/a", []data.PublicKey{aKey}, []string{""}),
		"error creating delegation")
	assert.NoError(t,
		ownerRepo.AddDelegation("targets/a/b", []data.PublicKey{bKey}, []string{""}),
		"error creating delegation")

	assert.NoError(t, ownerRepo.Publish())
	// delegation parents all get signed
	ownerRec.assertAsked(t, []string{data.CanonicalTargetsRole, "targets/a"})

	// delegated repo now publishes to delegated roles, but it will need
	// to download those roles first, since it doesn't know about them
	assertPublishToRolesSucceeds(t, delgRepo, []string{"targets/a/b"},
		[]string{"targets/a/b"})

	delgRec.assertAsked(t, []string{"targets/a/b"})
}

// TestDeleteRepo tests that local repo data, certificate, and keys are deleted from the client library call
func TestDeleteRepo(t *testing.T) {
	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// Assert initialization was successful before we delete
	assertRepoHasExpectedKeys(t, repo, rootKeyID, true)
	assertRepoHasExpectedCerts(t, repo)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalRootRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTargetsRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, true)

	// Delete all client trust data for repo
	err := repo.DeleteTrustData()
	assert.NoError(t, err)

	// Assert no metadata for this repo exists locally
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalRootRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTargetsRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTimestampRole, false)

	// Assert no certs for this repo exist locally
	_, err = repo.CertStore.GetCertificatesByCN(gun)
	assert.Error(t, err)
	assert.IsType(t, &trustmanager.ErrNoCertificatesFound{}, err)
	assert.NotNil(t, err)

	// Assert keys for this repo exist locally
	assertRepoHasExpectedKeys(t, repo, rootKeyID, true)
}

type brokenRemoveFilestore struct {
	store.MetadataStore
}

func (s *brokenRemoveFilestore) RemoveAll() error {
	return fmt.Errorf("can't remove from this broken filestore")
}

// TestDeleteRepoBadFilestore tests that we properly error when trying to remove against a faulty filestore
func TestDeleteRepoBadFilestore(t *testing.T) {
	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// Assert initialization was successful before we delete
	assertRepoHasExpectedKeys(t, repo, rootKeyID, true)
	assertRepoHasExpectedCerts(t, repo)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalRootRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTargetsRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, true)

	// Make the filestore faulty on remove
	repo.fileStore = &brokenRemoveFilestore{repo.fileStore}

	// Delete all client trust data for repo, assert an error on the filestore removal
	err := repo.DeleteTrustData()
	assert.Error(t, err)
}

// TestDeleteRepoNoCerts tests that local repo data is deleted successfully without an error even when we do not have certificates
func TestDeleteRepoNoCerts(t *testing.T) {
	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, gun, ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// Assert initialization was successful before we delete
	assertRepoHasExpectedKeys(t, repo, rootKeyID, true)
	assertRepoHasExpectedCerts(t, repo)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalRootRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTargetsRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, true)

	// Delete the certificate store contents and assert it has been fully deleted
	repo.CertStore.RemoveAll()
	_, err := repo.CertStore.GetCertificatesByCN(gun)
	assert.Error(t, err)
	assert.IsType(t, &trustmanager.ErrNoCertificatesFound{}, err)
	assert.NotNil(t, err)

	// Delete all client trust data for repo
	err = repo.DeleteTrustData()
	assert.NoError(t, err)

	// Assert no metadata for this repo exists locally
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalRootRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTargetsRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTimestampRole, false)

	// Assert no certs for this repo exist locally
	_, err = repo.CertStore.GetCertificatesByCN(gun)
	assert.Error(t, err)
	assert.IsType(t, &trustmanager.ErrNoCertificatesFound{}, err)
	assert.NotNil(t, err)

	// Assert keys for this repo exist locally
	assertRepoHasExpectedKeys(t, repo, rootKeyID, true)
}

// Test that we get a correct list of roles with keys and signatures
func TestListRoles(t *testing.T) {
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	assert.NoError(t, repo.Publish())

	rolesWithSigs, err := repo.ListRoles()
	assert.NoError(t, err)

	// Should only have base roles at this point
	assert.Len(t, rolesWithSigs, len(data.BaseRoles))
	// Each base role should only have one key, one signature, and its key should match the signature's key
	for _, role := range rolesWithSigs {
		assert.Len(t, role.Signatures, 1)
		assert.Len(t, role.KeyIDs, 1)
		assert.Equal(t, role.Signatures[0].KeyID, role.KeyIDs[0])
	}

	// Create a delegation on the top level
	aKey := createKey(t, repo, "user", true)
	assert.NoError(t,
		repo.AddDelegation("targets/a", []data.PublicKey{aKey}, []string{""}),
		"error creating delegation")

	assert.NoError(t, repo.Publish())

	rolesWithSigs, err = repo.ListRoles()
	assert.NoError(t, err)

	assert.Len(t, rolesWithSigs, len(data.BaseRoles)+1)
	// The delegation hasn't published any targets or metadata so it won't have a signature yet
	for _, role := range rolesWithSigs {
		if role.Name == "targets/a" {
			assert.Nil(t, role.Signatures)
		} else {
			assert.Len(t, role.Signatures, 1)
			assert.Equal(t, role.Signatures[0].KeyID, role.KeyIDs[0])
		}
		assert.Len(t, role.KeyIDs, 1)
	}

	addTarget(t, repo, "current", "../fixtures/intermediate-ca.crt", "targets/a")
	assert.NoError(t, repo.Publish())

	rolesWithSigs, err = repo.ListRoles()
	assert.NoError(t, err)

	assert.Len(t, rolesWithSigs, len(data.BaseRoles)+1)
	// The delegation should have a signature now
	for _, role := range rolesWithSigs {
		assert.Len(t, role.Signatures, 1)
		assert.Equal(t, role.Signatures[0].KeyID, role.KeyIDs[0])
		assert.Len(t, role.KeyIDs, 1)
	}

	// Create another delegation, one level further
	bKey := createKey(t, repo, "user", true)
	assert.NoError(t,
		repo.AddDelegation("targets/a/b", []data.PublicKey{bKey}, []string{""}),
		"error creating delegation")

	assert.NoError(t, repo.Publish())

	rolesWithSigs, err = repo.ListRoles()
	assert.NoError(t, err)

	assert.Len(t, rolesWithSigs, len(data.BaseRoles)+2)
	// The nested delegation hasn't published any targets or metadata so it won't have a signature yet
	for _, role := range rolesWithSigs {
		if role.Name == "targets/a/b" {
			assert.Nil(t, role.Signatures)
		} else {
			assert.Len(t, role.Signatures, 1)
			assert.Equal(t, role.Signatures[0].KeyID, role.KeyIDs[0])
		}
		assert.Len(t, role.KeyIDs, 1)
	}

	// Now make another repo and check that we don't pick up its roles
	repo2, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary2", ts.URL, false)
	defer os.RemoveAll(repo2.baseDir)

	assert.NoError(t, repo2.Publish())

	// repo2 only has the base roles
	rolesWithSigs2, err := repo2.ListRoles()
	assert.NoError(t, err)
	assert.Len(t, rolesWithSigs2, len(data.BaseRoles))

	// original repo stays in same state (base roles + 2 delegations)
	rolesWithSigs, err = repo.ListRoles()
	assert.NoError(t, err)
	assert.Len(t, rolesWithSigs, len(data.BaseRoles)+2)
}
