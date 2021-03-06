package client

import (
	"bytes"
	"crypto/rand"
	regJson "encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Sirupsen/logrus"
	ctxu "github.com/docker/distribution/context"
	"github.com/docker/notary/certs"
	"github.com/docker/notary/client/changelist"
	"github.com/docker/notary/cryptoservice"
	"github.com/docker/notary/passphrase"
	"github.com/docker/notary/server"
	"github.com/docker/notary/server/storage"
	"github.com/docker/notary/trustmanager"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/tuf/signed"
	"github.com/docker/notary/tuf/store"
	"github.com/docker/notary/tuf/validation"
	"github.com/jfrazelle/go/canonical/json"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
)

func simpleTestServer(t *testing.T) (
	*httptest.Server, *http.ServeMux, map[string]data.PrivateKey) {

	roles := []string{data.CanonicalTimestampRole, data.CanonicalSnapshotRole}
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

func initializeRepo(t *testing.T, rootType, tempBaseDir, gun, url string,
	serverManagesSnapshot bool) (*NotaryRepository, string) {

	serverManagedRoles := []string{}
	if serverManagesSnapshot {
		serverManagedRoles = []string{data.CanonicalSnapshotRole}
	}

	repo, rootPubKeyID := createRepoAndKey(t, rootType, tempBaseDir, gun, url)

	err := repo.Initialize(rootPubKeyID, serverManagedRoles...)
	assert.NoError(t, err, "error creating repository: %s", err)

	return repo, rootPubKeyID
}

// Creates a new repository and adds a root key.  Returns the repo and key ID.
func createRepoAndKey(t *testing.T, rootType, tempBaseDir, gun, url string) (
	*NotaryRepository, string) {

	repo, err := NewNotaryRepository(
		tempBaseDir, gun, url, http.DefaultTransport, passphraseRetriever)
	assert.NoError(t, err, "error creating repo: %s", err)

	rootPubKey, err := repo.CryptoService.Create("root", rootType)
	assert.NoError(t, err, "error generating root key: %s", err)

	return repo, rootPubKey.ID()
}

// Initializing a new repo while specifying that the server should manage the root
// role will fail.
func TestInitRepositoryManagedRolesIncludingRoot(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory")
	defer os.RemoveAll(tempBaseDir)

	repo, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", "http://localhost")
	err = repo.Initialize(rootPubKeyID, data.CanonicalRootRole)
	assert.Error(t, err)
	assert.IsType(t, ErrInvalidRemoteRole{}, err)
	// Just testing the error message here in this one case
	assert.Equal(t, err.Error(),
		"notary does not support the server managing the root key")
}

// Initializing a new repo while specifying that the server should manage some
// invalid role will fail.
func TestInitRepositoryManagedRolesInvalidRole(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory")
	defer os.RemoveAll(tempBaseDir)

	repo, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", "http://localhost")
	err = repo.Initialize(rootPubKeyID, "randomrole")
	assert.Error(t, err)
	assert.IsType(t, ErrInvalidRemoteRole{}, err)
}

// Initializing a new repo while specifying that the server should manage the
// targets role will fail.
func TestInitRepositoryManagedRolesIncludingTargets(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory")
	defer os.RemoveAll(tempBaseDir)

	repo, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", "http://localhost")
	err = repo.Initialize(rootPubKeyID, data.CanonicalTargetsRole)
	assert.Error(t, err)
	assert.IsType(t, ErrInvalidRemoteRole{}, err)
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

	repo, rootPubKeyID := createRepoAndKey(
		t, data.ECDSAKey, tempBaseDir, "docker.com/notary", ts.URL)
	err = repo.Initialize(rootPubKeyID, data.CanonicalTimestampRole)
	assert.NoError(t, err)
}

// passing timestamp + snapshot, or just snapshot, is tested in the next two
// test cases.

// TestInitRepoServerOnlyManagesTimestampKey runs through the process of
// initializing a repository and makes sure the repository looks correct on disk.
// We test this with both an RSA and ECDSA root key.
// This test case covers the default case where the server only manages the
// timestamp key.
func TestInitRepoServerOnlyManagesTimestampKey(t *testing.T) {
	testInitRepo(t, data.ECDSAKey, false)
	if !testing.Short() {
		testInitRepo(t, data.RSAKey, false)
	}
}

// TestInitRepoServerManagesTimestampAndSnapshotKeys runs through the process of
// initializing a repository and makes sure the repository looks correct on disk.
// We test this with both an RSA and ECDSA root key.
// This test case covers the server managing both the timestap and snapshot keys.
func TestInitRepoServerManagesTimestampAndSnapshotKeys(t *testing.T) {
	testInitRepo(t, data.ECDSAKey, true)
	if !testing.Short() {
		testInitRepo(t, data.RSAKey, true)
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

// This creates a new certificate manager in the repo's base directory and
// makes sure the repo has the right certificates
func assertRepoHasExpectedCerts(t *testing.T, repo *NotaryRepository) {
	// The repo should have a certificate manager and have created certs using
	// it, so create a new manager, and check that the certs do exist and
	// are valid
	certManager, err := certs.NewManager(repo.baseDir)
	assert.NoError(t, err)
	certificates := certManager.TrustedCertificateStore().GetCertificates()
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
		assert.Len(t, decodedRoot.Keys, len(data.ValidRoles),
			"wrong number of keys in root.json")
		assert.Len(t, decodedRoot.Roles, len(data.ValidRoles),
			"wrong number of roles in root.json")

		for role := range data.ValidRoles {
			_, ok := decodedRoot.Roles[role]
			assert.True(t, ok, "Missing role %s in root.json", role)
		}
	}
}

func testInitRepo(t *testing.T, rootType string, serverManagesSnapshot bool) {
	gun := "docker.com/notary"
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, rootType, tempBaseDir, gun, ts.URL,
		serverManagesSnapshot)

	assertRepoHasExpectedKeys(t, repo, rootKeyID, !serverManagesSnapshot)
	assertRepoHasExpectedCerts(t, repo)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalRootRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTargetsRole, true)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole,
		!serverManagesSnapshot)
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
	err = repo.AddTarget(target, roles...)
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
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)

	testAddOrDeleteTarget(t, repo, changelist.ActionCreate, nil,
		[]string{data.CanonicalTargetsRole})
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
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)

	roleName := filepath.Join(data.CanonicalTargetsRole, "a")
	testAddOrDeleteTarget(t, repo, changelist.ActionCreate,
		[]string{
			strings.ToUpper(data.CanonicalTargetsRole),
			strings.ToUpper(roleName),
		},
		[]string{data.CanonicalTargetsRole, roleName})
}

// TestAddTargetToSpecifiedInvalidRoles expects errors to be returned if
// adding a target to an invalid role.  If any of the roles are invalid,
// no targets are added to any roles.
func TestAddTargetToSpecifiedInvalidRoles(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)
	defer os.RemoveAll(tempBaseDir)

	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)

	invalidRoles := []string{
		data.CanonicalRootRole,
		data.CanonicalSnapshotRole,
		data.CanonicalTimestampRole,
		"target/otherrole",
		"otherrole",
	}

	for _, invalidRole := range invalidRoles {
		target, err := NewTarget("latest", "../fixtures/intermediate-ca.crt")
		assert.NoError(t, err, "error creating target")

		err = repo.AddTarget(target, data.CanonicalTargetsRole, invalidRole)
		assert.Error(t, err, "Expected an ErrInvalidRole error")
		assert.IsType(t, data.ErrInvalidRole{}, err)

		changes := getChanges(t, repo)
		assert.Len(t, changes, 0)
	}
}

// General way to assert that errors writing a changefile are propagated up
func testErrorWritingChangefiles(t *testing.T, writeChangeFile func(*NotaryRepository) error) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)

	// first, make the actual changefile unwritable by making the changelist
	// directory unwritable
	changelistPath := filepath.Join(repo.tufRepoPath, "changelist")
	err = os.MkdirAll(changelistPath, 0744)
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
		return repo.AddTarget(target, data.CanonicalTargetsRole)
	})
}

// TestRemoveTargetToTargetRoleByDefault removes a target without specifying a
// role from a repo.  Confirms that the changelist is created correctly for
// the targets scope.
func TestRemoveTargetToTargetRoleByDefault(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)

	testAddOrDeleteTarget(t, repo, changelist.ActionDelete, nil,
		[]string{data.CanonicalTargetsRole})
}

// TestRemoveTargetFromSpecifiedValidRoles removes a target from the specified
// roles. Confirms that the changelist is created correctly, one for each of
// the the specified roles as scopes.
func TestRemoveTargetFromSpecifiedValidRoles(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)

	roleName := filepath.Join(data.CanonicalTargetsRole, "a")
	testAddOrDeleteTarget(t, repo, changelist.ActionDelete,
		[]string{
			strings.ToUpper(data.CanonicalTargetsRole),
			strings.ToUpper(roleName),
		},
		[]string{data.CanonicalTargetsRole, roleName})
}

// TestRemoveTargetFromSpecifiedInvalidRoles expects errors to be returned if
// removing a target to an invalid role.  If any of the roles are invalid,
// no targets are removed from any roles.
func TestRemoveTargetToSpecifiedInvalidRoles(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)
	defer os.RemoveAll(tempBaseDir)

	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)

	invalidRoles := []string{
		data.CanonicalRootRole,
		data.CanonicalSnapshotRole,
		data.CanonicalTimestampRole,
		"target/otherrole",
		"otherrole",
	}

	for _, invalidRole := range invalidRoles {
		err = repo.RemoveTarget("latest", data.CanonicalTargetsRole, invalidRole)
		assert.Error(t, err, "Expected an ErrInvalidRole error")
		assert.IsType(t, data.ErrInvalidRole{}, err)

		changes := getChanges(t, repo)
		assert.Len(t, changes, 0)
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
	if !testing.Short() {
		testListEmptyTargets(t, data.RSAKey)
		testListTarget(t, data.RSAKey)
	}
}

func testListEmptyTargets(t *testing.T, rootType string) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"

	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, tempBaseDir, gun, ts.URL, false)

	_, err = repo.ListTargets()
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

	signedSnapshot, err := savedTUFRepo.SignSnapshot(
		data.DefaultExpires("snapshot"))
	assert.NoError(t, err)

	signedTimestamp, err := savedTUFRepo.SignTimestamp(
		data.DefaultExpires("timestamp"))
	assert.NoError(t, err)

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/root.json",
		func(w http.ResponseWriter, r *http.Request) {
			assert.NoError(t, err)
			fmt.Fprint(w, string(rootFileBytes))
		})

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/timestamp.json",
		func(w http.ResponseWriter, r *http.Request) {
			timestampJSON, _ := json.Marshal(signedTimestamp)
			fmt.Fprint(w, string(timestampJSON))
		})

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/snapshot.json",
		func(w http.ResponseWriter, r *http.Request) {
			snapshotJSON, _ := json.Marshal(signedSnapshot)
			fmt.Fprint(w, string(snapshotJSON))
		})

	mux.HandleFunc("/v2/docker.com/notary/_trust/tuf/targets.json",
		func(w http.ResponseWriter, r *http.Request) {
			targetsJSON, _ := json.Marshal(signedTargets)
			fmt.Fprint(w, string(targetsJSON))
		})
}

// We want to sort by name, so we can guarantee ordering.
type targetSorter []*Target

func (k targetSorter) Len() int           { return len(k) }
func (k targetSorter) Swap(i, j int)      { k[i], k[j] = k[j], k[i] }
func (k targetSorter) Less(i, j int) bool { return k[i].Name < k[j].Name }

func testListTarget(t *testing.T, rootType string) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"

	ts, mux, keys := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, tempBaseDir, gun, ts.URL, false)

	// tests need to manually boostrap timestamp as client doesn't generate it
	err = repo.tufRepo.InitTimestamp()
	assert.NoError(t, err, "error creating repository: %s", err)

	latestTarget := addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt")
	currentTarget := addTarget(t, repo, "current", "../fixtures/intermediate-ca.crt")

	// Apply the changelist. Normally, this would be done by Publish

	// load the changelist for this repo
	cl, err := changelist.NewFileChangelist(
		filepath.Join(tempBaseDir, "tuf", filepath.FromSlash(gun), "changelist"))
	assert.NoError(t, err, "could not open changelist")

	// apply the changelist to the repo
	err = applyChangelist(repo.tufRepo, cl)
	assert.NoError(t, err, "could not apply changelist")

	fakeServerData(t, repo, mux, keys)

	targets, err := repo.ListTargets()
	assert.NoError(t, err)

	// Should be two targets
	assert.Len(t, targets, 2, "unexpected number of targets returned by ListTargets")

	sort.Stable(targetSorter(targets))

	// current should be first
	assert.Equal(t, currentTarget, targets[0], "current target does not match")
	assert.Equal(t, latestTarget, targets[1], "latest target does not match")

	// Also test GetTargetByName
	newLatestTarget, err := repo.GetTargetByName("latest")
	assert.NoError(t, err)
	assert.Equal(t, latestTarget, newLatestTarget, "latest target does not match")

	newCurrentTarget, err := repo.GetTargetByName("current")
	assert.NoError(t, err)
	assert.Equal(t, currentTarget, newCurrentTarget, "current target does not match")
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
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"

	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	initializeRepo(t, rootType, tempBaseDir, gun, ts.URL, false)

	rootJSONFile := filepath.Join(tempBaseDir, "tuf", filepath.FromSlash(gun), "metadata", "root.json")

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
		if key, ok := decodedRoot.Keys[keyid]; !ok {
			t.Fatal("key id not found in keys")
		} else {
			_, err := trustmanager.LoadCertFromPEM(key.Public())
			assert.NoError(t, err, "key is not a valid cert")
		}
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
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, tempBaseDir, gun, ts.URL, false)
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

// Create a repo, instantiate a notary server, and publish the repo to the
// server, signing all the non-timestamp metadata.
// We test this with both an RSA and ECDSA root key
func TestPublishClientHasSnapshotKey(t *testing.T) {
	testPublish(t, data.ECDSAKey, false)
	if !testing.Short() {
		testPublish(t, data.RSAKey, false)
	}
}

// Create a repo, instantiate a notary server (designating the server as the
// snapshot signer) , and publish the repo to the server, signing the root and
// targets metadata only.  The server should sign just fine.
// We test this with both an RSA and ECDSA root key
func TestPublishAfterInitServerHasSnapshotKey(t *testing.T) {
	testPublish(t, data.ECDSAKey, true)
	if !testing.Short() {
		testPublish(t, data.RSAKey, true)
	}
}

func testPublish(t *testing.T, rootType string, serverManagesSnapshot bool) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, tempBaseDir, gun, ts.URL,
		serverManagesSnapshot)
	assertPublishSucceeds(t, repo)
}

func assertPublishSucceeds(t *testing.T, repo1 *NotaryRepository) {
	// Create 2 targets
	latestTarget := addTarget(t, repo1, "latest", "../fixtures/intermediate-ca.crt")
	currentTarget := addTarget(t, repo1, "current", "../fixtures/intermediate-ca.crt")
	assert.Len(t, getChanges(t, repo1), 2, "wrong number of changelist files found")

	// Now test Publish
	err := repo1.Publish()
	assert.NoError(t, err)
	assert.Len(t, getChanges(t, repo1), 0, "wrong number of changelist files found")

	// Create a new repo and pull from the server
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	repo2, err := NewNotaryRepository(tempBaseDir, repo1.gun, repo1.baseURL,
		http.DefaultTransport, passphraseRetriever)
	assert.NoError(t, err, "error creating repository: %s", err)

	// Should be two targets
	for _, repo := range []*NotaryRepository{repo1, repo2} {
		targets, err := repo.ListTargets()
		assert.NoError(t, err)

		assert.Len(t, targets, 2, "unexpected number of targets returned by ListTargets")

		sort.Stable(targetSorter(targets))

		assert.Equal(t, currentTarget, targets[0], "current target does not match")
		assert.Equal(t, latestTarget, targets[1], "latest target does not match")

		// Also test GetTargetByName
		newLatestTarget, err := repo.GetTargetByName("latest")
		assert.NoError(t, err)
		assert.Equal(t, latestTarget, newLatestTarget, "latest target does not match")

		newCurrentTarget, err := repo.GetTargetByName("current")
		assert.NoError(t, err)
		assert.Equal(t, currentTarget, newCurrentTarget, "current target does not match")
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
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, rootType, tempBaseDir, gun, ts.URL, true)
	// no timestamp metadata because that comes from the server
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTimestampRole, false)
	// no snapshot metadata because that comes from the server
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, false)

	// Publish something
	published := addTarget(t, repo, "v1", "../fixtures/intermediate-ca.crt")
	err = repo.Publish()
	assert.NoError(t, err)
	// still no timestamp or snapshot metadata info
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalTimestampRole, false)
	assertRepoHasExpectedMetadata(t, repo, data.CanonicalSnapshotRole, false)

	// list, so that the snapshot metadata is pulled from server
	targets, err := repo.ListTargets()
	assert.NoError(t, err)
	assert.Equal(t, []*Target{published}, targets)
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
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	// create repo and delete the snapshot key and metadata
	repo, _ := initializeRepo(t, rootType, tempBaseDir, gun, ts.URL, false)
	snapshotRole, ok := repo.tufRepo.Root.Signed.Roles[data.CanonicalSnapshotRole]
	assert.True(t, ok)
	for _, keyID := range snapshotRole.KeyIDs {
		repo.CryptoService.RemoveKey(keyID)
	}

	// Publish something
	addTarget(t, repo, "v1", "../fixtures/intermediate-ca.crt")
	err = repo.Publish()
	assert.Error(t, err)
	assert.IsType(t, validation.ErrBadHierarchy{}, err)
}

// If the snapshot metadata is corrupt, whether the client or server has the
// snapshot key, we can't publish.
// We test this with both an RSA and ECDSA root key
func TestPublishSnapshotCorrupt(t *testing.T) {
	testPublishBadExistingSnapshot(t, data.ECDSAKey, true, true)
	testPublishBadExistingSnapshot(t, data.ECDSAKey, false, true)
}

// If the snapshot metadata is unreadable, whether the client or server has the
// snapshot key, we can't publish.
// We test this with both an RSA and ECDSA root key
func TestPublishSnapshotUnreadable(t *testing.T) {
	testPublishBadExistingSnapshot(t, data.ECDSAKey, true, false)
	testPublishBadExistingSnapshot(t, data.ECDSAKey, false, false)
}

func testPublishBadExistingSnapshot(t *testing.T, rootType string,
	serverManagesSnapshot bool, readable bool) {

	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(
		t, rootType, tempBaseDir, gun, ts.URL, serverManagesSnapshot)

	addTarget(t, repo, "v1", "../fixtures/intermediate-ca.crt")

	var expectedErrType interface{}
	if readable {
		// write a corrupt snapshots file
		repo.fileStore.SetMeta(data.CanonicalSnapshotRole, []byte("this isn't JSON"))
		expectedErrType = &regJson.SyntaxError{}
	} else {
		// create a directory instead of a file
		path := fmt.Sprintf("%s.%s",
			filepath.Join(tempBaseDir, tufDir, filepath.FromSlash(gun),
				"metadata", data.CanonicalSnapshotRole), "json")
		os.RemoveAll(path)
		err := os.Mkdir(path, 0755)
		defer os.RemoveAll(path)
		assert.NoError(t, err)

		expectedErrType = &os.PathError{}
	}
	err = repo.Publish()
	assert.Error(t, err)
	assert.IsType(t, expectedErrType, err)
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

// Rotate invalid roles, or attempt to delegate target signing to the server
func TestRotateKeyInvalidRole(t *testing.T) {
	tempBaseDir, err := ioutil.TempDir("/tmp", "notary-test-")
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)
	defer os.RemoveAll(tempBaseDir)

	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)

	// the equivalent of: (root, true), (root, false), (timestamp, true),
	// (timestamp, false), (targets, true)
	for role := range data.ValidRoles {
		if role == data.CanonicalSnapshotRole {
			continue
		}
		for _, serverManagesKey := range []bool{true, false} {
			if role == data.CanonicalTargetsRole && !serverManagesKey {
				continue
			}
			err = repo.RotateKey(role, serverManagesKey)
			assert.Error(t, err,
				"Rotating a %s key with server-managing the key as %v should fail",
				role, serverManagesKey)
		}
	}
}

// Rotates the keys.  After the rotation, downloading the latest metadata
// and assert that the keys have changed
func assertRotationSuccessful(t *testing.T, repo *NotaryRepository,
	keysToRotate map[string]bool) {

	oldKeyIDs := make(map[string][]string)
	for role := range keysToRotate {
		keyIDs := repo.tufRepo.Root.Signed.Roles[role].KeyIDs
		oldKeyIDs[role] = keyIDs
	}

	// Do rotation
	for role, serverManaged := range keysToRotate {
		assert.NoError(t, repo.RotateKey(role, serverManaged))
	}

	// Publish
	err := repo.Publish()
	assert.NoError(t, err)

	// Get root.json. Check keys have changed.
	_, err = repo.GetTargetByName("latest") // force a pull
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

		// the new key is present in the cryptoservice, or not present if remote
		key, _, err := repo.CryptoService.GetPrivateKey(keyIDs[0])
		if isRemoteKey {
			assert.Error(t, err)
			assert.Nil(t, key)
		} else {
			assert.NoError(t, err)
			assert.NotNil(t, key)
		}
	}

	// Confirm changelist dir empty after publishing changes
	changes := getChanges(t, repo)
	assert.Len(t, changes, 0, "wrong number of changelist files found")
}

// Initialize repo to have the server sign snapshots (remote snapshot key)
// Without downloading a server-signed snapshot file, rotate keys so that
//    snapshots are locally signed (local snapshot key)
// Assert that we can publish.
func TestRotateBeforePublishFromRemoteKeyToLocalKey(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, true)
	// Adding a target will allow us to confirm the repository is still valid
	// after rotating the keys.
	addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt")
	assertRotationSuccessful(t, repo, map[string]bool{
		data.CanonicalTargetsRole:  false,
		data.CanonicalSnapshotRole: false})
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

	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)

	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts := fullTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL,
		serverManagesSnapshotInit)

	// Adding a target will allow us to confirm the repository is still valid after
	// rotating the keys.
	addTarget(t, repo, "latest", "../fixtures/intermediate-ca.crt")

	// Publish
	err = repo.Publish()
	assert.NoError(t, err)

	// Get root.json and capture targets + snapshot key IDs
	repo.GetTargetByName("latest") // force a pull
	assertRotationSuccessful(t, repo, keysToRotate)
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

	_, err = repo.ListTargets()
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
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)
	targetKeyIds := repo.CryptoService.ListKeys(data.CanonicalTargetsRole)
	assert.NotEmpty(t, targetKeyIds)
	targetPubKey := repo.CryptoService.GetKey(targetKeyIds[0])
	assert.NotNil(t, targetPubKey)

	err = repo.AddDelegation("root", 1, []data.PublicKey{targetPubKey})
	assert.Error(t, err)
	assert.IsType(t, data.ErrInvalidRole{}, err)
	assert.Empty(t, getChanges(t, repo))

	// to show that adding does not care about the hierarchy
	err = repo.AddDelegation("targets/a/b/c", 1, []data.PublicKey{targetPubKey})
	assert.NoError(t, err)

	// ensure that the changefiles is correct
	changes := getChanges(t, repo)
	assert.Len(t, changes, 1)
	assert.Equal(t, changelist.ActionCreate, changes[0].Action())
	assert.Equal(t, "targets/a/b/c", changes[0].Scope())
	assert.Equal(t, changelist.TypeTargetsDelegation, changes[0].Type())
	assert.Equal(t, "", changes[0].Path())
	assert.NotEmpty(t, changes[0].Content())
}

// The changefile produced by AddDelegation, when applied, actually adds
// the delegation to the repo (assuming the delegation hierarchy is correct -
// tests for change application validation are in helpers_test.go)
func TestAddDelegationChangefileApplicable(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)
	targetKeyIds := repo.CryptoService.ListKeys(data.CanonicalTargetsRole)
	assert.NotEmpty(t, targetKeyIds)
	targetPubKey := repo.CryptoService.GetKey(targetKeyIds[0])
	assert.NotNil(t, targetPubKey)

	// this hierarchy has to be right to be applied
	err = repo.AddDelegation("targets/a", 1, []data.PublicKey{targetPubKey})
	assert.NoError(t, err)
	changes := getChanges(t, repo)
	assert.Len(t, changes, 1)

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

		return repo.AddDelegation("targets/a", 1, []data.PublicKey{targetPubKey})
	})
}

// RemoveDelegation rejects attempts to remove invalidly-named delegations,
// but otherwise does not validate the name of the delegation to remove.  This
// test ensures that the changefile generated by RemoveDelegation is correct.
func TestRemoveDelegationChangefileValid(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)
	rootPubKey := repo.CryptoService.GetKey(rootKeyID)
	assert.NotNil(t, rootPubKey)

	err = repo.RemoveDelegation("root")
	assert.Error(t, err)
	assert.IsType(t, data.ErrInvalidRole{}, err)
	assert.Empty(t, getChanges(t, repo))

	// to demonstrate that so long as the delegation name is valid, the
	// existence of the delegation doesn't matter
	assert.NoError(t, repo.RemoveDelegation("targets/a/b/c"))

	// ensure that the changefile is correct
	changes := getChanges(t, repo)
	assert.Len(t, changes, 1)
	assert.Equal(t, changelist.ActionDelete, changes[0].Action())
	assert.Equal(t, "targets/a/b/c", changes[0].Scope())
	assert.Equal(t, changelist.TypeTargetsDelegation, changes[0].Type())
	assert.Equal(t, "", changes[0].Path())
	assert.Empty(t, changes[0].Content())
}

// The changefile produced by RemoveDelegation, when applied, actually removes
// the delegation from the repo (assuming the repo exists - tests for
// change application validation are in helpers_test.go)
func TestRemoveDelegationChangefileApplicable(t *testing.T) {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	defer os.RemoveAll(tempBaseDir)
	assert.NoError(t, err, "failed to create a temporary directory: %s", err)

	gun := "docker.com/notary"
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, rootKeyID := initializeRepo(t, data.ECDSAKey, tempBaseDir, gun, ts.URL, false)
	rootPubKey := repo.CryptoService.GetKey(rootKeyID)
	assert.NotNil(t, rootPubKey)

	// add a delegation first so it can be removed
	assert.NoError(t, repo.AddDelegation("targets/a", 1, []data.PublicKey{rootPubKey}))
	changes := getChanges(t, repo)
	assert.Len(t, changes, 1)
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[0]))

	targetRole := repo.tufRepo.Targets[data.CanonicalTargetsRole]
	assert.Len(t, targetRole.Signed.Delegations.Roles, 1)
	assert.Len(t, targetRole.Signed.Delegations.Keys, 1)

	// now remove it
	assert.NoError(t, repo.RemoveDelegation("targets/a"))
	changes = getChanges(t, repo)
	assert.Len(t, changes, 2)
	assert.NoError(t, applyTargetsChange(repo.tufRepo, changes[1]))

	targetRole = repo.tufRepo.Targets[data.CanonicalTargetsRole]
	assert.Empty(t, targetRole.Signed.Delegations.Roles)
	assert.Empty(t, targetRole.Signed.Delegations.Keys)
}

// TestRemoveDelegationErrorWritingChanges expects errors writing a change to
// file to be propagated.
func TestRemoveDelegationErrorWritingChanges(t *testing.T) {
	testErrorWritingChangefiles(t, func(repo *NotaryRepository) error {
		return repo.RemoveDelegation("targets/a")
	})
}
