package client

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/docker/notary/client/changelist"
	tuf "github.com/docker/notary/tuf"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/tuf/keys"
	"github.com/docker/notary/tuf/testutils"
	"github.com/stretchr/testify/assert"
)

func TestApplyTargetsChange(t *testing.T) {
	kdb := keys.NewDB()
	role, err := data.NewRole("targets", 1, nil, nil, nil)
	assert.NoError(t, err)
	kdb.AddRole(role)

	repo := tuf.NewRepo(kdb, nil)
	err = repo.InitTargets(data.CanonicalTargetsRole)
	assert.NoError(t, err)
	hash := sha256.Sum256([]byte{})
	f := &data.FileMeta{
		Length: 1,
		Hashes: map[string][]byte{
			"sha256": hash[:],
		},
	}
	fjson, err := json.Marshal(f)
	assert.NoError(t, err)

	addChange := &changelist.TufChange{
		Actn:       changelist.ActionCreate,
		Role:       changelist.ScopeTargets,
		ChangeType: "target",
		ChangePath: "latest",
		Data:       fjson,
	}
	err = applyTargetsChange(repo, addChange)
	assert.NoError(t, err)
	assert.NotNil(t, repo.Targets["targets"].Signed.Targets["latest"])

	removeChange := &changelist.TufChange{
		Actn:       changelist.ActionDelete,
		Role:       changelist.ScopeTargets,
		ChangeType: "target",
		ChangePath: "latest",
		Data:       nil,
	}
	err = applyTargetsChange(repo, removeChange)
	assert.NoError(t, err)
	_, ok := repo.Targets["targets"].Signed.Targets["latest"]
	assert.False(t, ok)
}

func TestApplyChangelist(t *testing.T) {
	kdb := keys.NewDB()
	role, err := data.NewRole("targets", 1, nil, nil, nil)
	assert.NoError(t, err)
	kdb.AddRole(role)

	repo := tuf.NewRepo(kdb, nil)
	err = repo.InitTargets(data.CanonicalTargetsRole)
	assert.NoError(t, err)
	hash := sha256.Sum256([]byte{})
	f := &data.FileMeta{
		Length: 1,
		Hashes: map[string][]byte{
			"sha256": hash[:],
		},
	}
	fjson, err := json.Marshal(f)
	assert.NoError(t, err)

	cl := changelist.NewMemChangelist()
	addChange := &changelist.TufChange{
		Actn:       changelist.ActionCreate,
		Role:       changelist.ScopeTargets,
		ChangeType: "target",
		ChangePath: "latest",
		Data:       fjson,
	}
	cl.Add(addChange)
	err = applyChangelist(repo, cl)
	assert.NoError(t, err)
	assert.NotNil(t, repo.Targets["targets"].Signed.Targets["latest"])

	cl.Clear("")

	removeChange := &changelist.TufChange{
		Actn:       changelist.ActionDelete,
		Role:       changelist.ScopeTargets,
		ChangeType: "target",
		ChangePath: "latest",
		Data:       nil,
	}
	cl.Add(removeChange)
	err = applyChangelist(repo, cl)
	assert.NoError(t, err)
	_, ok := repo.Targets["targets"].Signed.Targets["latest"]
	assert.False(t, ok)
}

func TestApplyChangelistMulti(t *testing.T) {
	kdb := keys.NewDB()
	role, err := data.NewRole("targets", 1, nil, nil, nil)
	assert.NoError(t, err)
	kdb.AddRole(role)

	repo := tuf.NewRepo(kdb, nil)
	err = repo.InitTargets(data.CanonicalTargetsRole)
	assert.NoError(t, err)
	hash := sha256.Sum256([]byte{})
	f := &data.FileMeta{
		Length: 1,
		Hashes: map[string][]byte{
			"sha256": hash[:],
		},
	}
	fjson, err := json.Marshal(f)
	assert.NoError(t, err)

	cl := changelist.NewMemChangelist()
	addChange := &changelist.TufChange{
		Actn:       changelist.ActionCreate,
		Role:       changelist.ScopeTargets,
		ChangeType: "target",
		ChangePath: "latest",
		Data:       fjson,
	}

	removeChange := &changelist.TufChange{
		Actn:       changelist.ActionDelete,
		Role:       changelist.ScopeTargets,
		ChangeType: "target",
		ChangePath: "latest",
		Data:       nil,
	}

	cl.Add(addChange)
	cl.Add(removeChange)

	err = applyChangelist(repo, cl)
	assert.NoError(t, err)
	_, ok := repo.Targets["targets"].Signed.Targets["latest"]
	assert.False(t, ok)
}

func TestApplyTargetsDelegationCreateDelete(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	tgts := repo.Targets[data.CanonicalTargetsRole]
	assert.Len(t, tgts.Signed.Delegations.Roles, 1)
	assert.Len(t, tgts.Signed.Delegations.Keys, 1)

	_, ok := tgts.Signed.Delegations.Keys[newKey.ID()]
	assert.True(t, ok)

	role := tgts.Signed.Delegations.Roles[0]
	assert.Len(t, role.KeyIDs, 1)
	assert.Equal(t, newKey.ID(), role.KeyIDs[0])
	assert.Equal(t, "targets/level1", role.Name)
	assert.Equal(t, "level1", role.Paths[0])

	// delete delegation
	ch = changelist.NewTufChange(
		changelist.ActionDelete,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		nil,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	assert.Len(t, tgts.Signed.Delegations.Roles, 0)
	assert.Len(t, tgts.Signed.Delegations.Keys, 0)
}

func TestApplyTargetsDelegationCreate2SharedKey(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create first delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	// create second delegation
	kl = data.KeyList{newKey}
	td = &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level2"},
	}

	tdJSON, err = json.Marshal(td)
	assert.NoError(t, err)

	ch = changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level2",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	tgts := repo.Targets[data.CanonicalTargetsRole]
	assert.Len(t, tgts.Signed.Delegations.Roles, 2)
	assert.Len(t, tgts.Signed.Delegations.Keys, 1)

	role1 := tgts.Signed.Delegations.Roles[0]
	assert.Len(t, role1.KeyIDs, 1)
	assert.Equal(t, newKey.ID(), role1.KeyIDs[0])
	assert.Equal(t, "targets/level1", role1.Name)
	assert.Equal(t, "level1", role1.Paths[0])

	role2 := tgts.Signed.Delegations.Roles[1]
	assert.Len(t, role2.KeyIDs, 1)
	assert.Equal(t, newKey.ID(), role2.KeyIDs[0])
	assert.Equal(t, "targets/level2", role2.Name)
	assert.Equal(t, "level2", role2.Paths[0])

	// delete one delegation, ensure key remains
	ch = changelist.NewTufChange(
		changelist.ActionDelete,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		nil,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	assert.Len(t, tgts.Signed.Delegations.Roles, 1)
	assert.Len(t, tgts.Signed.Delegations.Keys, 1)

	// delete other delegation, ensure key cleaned up
	ch = changelist.NewTufChange(
		changelist.ActionDelete,
		"targets/level2",
		changelist.TypeTargetsDelegation,
		"",
		nil,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	assert.Len(t, tgts.Signed.Delegations.Roles, 0)
	assert.Len(t, tgts.Signed.Delegations.Keys, 0)
}

func TestApplyTargetsDelegationCreateEdit(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	// edit delegation
	newKey2, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	kl = data.KeyList{newKey2}
	td = &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		RemoveKeys:   []string{newKey.ID()},
	}

	tdJSON, err = json.Marshal(td)
	assert.NoError(t, err)

	ch = changelist.NewTufChange(
		changelist.ActionUpdate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	tgts := repo.Targets[data.CanonicalTargetsRole]
	assert.Len(t, tgts.Signed.Delegations.Roles, 1)
	assert.Len(t, tgts.Signed.Delegations.Keys, 1)

	_, ok := tgts.Signed.Delegations.Keys[newKey2.ID()]
	assert.True(t, ok)

	role := tgts.Signed.Delegations.Roles[0]
	assert.Len(t, role.KeyIDs, 1)
	assert.Equal(t, newKey2.ID(), role.KeyIDs[0])
	assert.Equal(t, "targets/level1", role.Name)
	assert.Equal(t, "level1", role.Paths[0])
}

func TestApplyTargetsDelegationEditNonExisting(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionUpdate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.Error(t, err)
	assert.IsType(t, data.ErrNoSuchRole{}, err)
}

func TestApplyTargetsDelegationCreateAlreadyExisting(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)
	// we have sufficient checks elsewhere we don't need to confirm that
	// creating fresh works here via more asserts.

	// when attempting to create the same role again, assert we receive
	// an ErrInvalidRole because an existing role can't be "created"
	err = applyTargetsChange(repo, ch)
	assert.Error(t, err)
	assert.IsType(t, data.ErrInvalidRole{}, err)
}

func TestApplyTargetsDelegationInvalidRole(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"bad role",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.Error(t, err)
}

func TestApplyTargetsDelegationInvalidJSONContent(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON[1:],
	)

	err = applyTargetsChange(repo, ch)
	assert.Error(t, err)
}

func TestApplyTargetsDelegationInvalidAction(t *testing.T) {
	_, repo, _ := testutils.EmptyRepo()

	ch := changelist.NewTufChange(
		"bad action",
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		nil,
	)

	err := applyTargetsChange(repo, ch)
	assert.Error(t, err)
}

func TestApplyTargetsChangeInvalidType(t *testing.T) {
	_, repo, _ := testutils.EmptyRepo()

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		"badType",
		"",
		nil,
	)

	err := applyTargetsChange(repo, ch)
	assert.Error(t, err)
}

// A delegated role MUST NOT have both Paths and PathHashPrefixes defined.
// These next 2 tests check that attempting to edit an existing role to
// create an invalid role errors in both possible combinations.
func TestApplyTargetsDelegationConflictPathsPrefixes(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	// add prefixes and update
	td.AddPathHashPrefixes = []string{"abc"}

	tdJSON, err = json.Marshal(td)
	assert.NoError(t, err)

	ch = changelist.NewTufChange(
		changelist.ActionUpdate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.Error(t, err)
}

func TestApplyTargetsDelegationConflictPrefixesPaths(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold:        1,
		AddKeys:             kl,
		AddPathHashPrefixes: []string{"abc"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	// add paths and update
	td.AddPaths = []string{"level1"}

	tdJSON, err = json.Marshal(td)
	assert.NoError(t, err)

	ch = changelist.NewTufChange(
		changelist.ActionUpdate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.Error(t, err)
}

func TestApplyTargetsDelegationCreateInvalid(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold:        1,
		AddKeys:             kl,
		AddPaths:            []string{"level1"},
		AddPathHashPrefixes: []string{"abc"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.Error(t, err)
}

func TestApplyTargetsDelegationCreate2Deep(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	newKey, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1"},
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	tgts := repo.Targets[data.CanonicalTargetsRole]
	assert.Len(t, tgts.Signed.Delegations.Roles, 1)
	assert.Len(t, tgts.Signed.Delegations.Keys, 1)

	_, ok := tgts.Signed.Delegations.Keys[newKey.ID()]
	assert.True(t, ok)

	role := tgts.Signed.Delegations.Roles[0]
	assert.Len(t, role.KeyIDs, 1)
	assert.Equal(t, newKey.ID(), role.KeyIDs[0])
	assert.Equal(t, "targets/level1", role.Name)
	assert.Equal(t, "level1", role.Paths[0])

	// init delegations targets file. This would be done as part of a publish
	// operation
	repo.InitTargets("targets/level1")

	td = &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
		AddPaths:     []string{"level1/level2"},
	}

	tdJSON, err = json.Marshal(td)
	assert.NoError(t, err)

	ch = changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1/level2",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.NoError(t, err)

	tgts = repo.Targets["targets/level1"]
	assert.Len(t, tgts.Signed.Delegations.Roles, 1)
	assert.Len(t, tgts.Signed.Delegations.Keys, 1)

	_, ok = tgts.Signed.Delegations.Keys[newKey.ID()]
	assert.True(t, ok)

	role = tgts.Signed.Delegations.Roles[0]
	assert.Len(t, role.KeyIDs, 1)
	assert.Equal(t, newKey.ID(), role.KeyIDs[0])
	assert.Equal(t, "targets/level1/level2", role.Name)
	assert.Equal(t, "level1/level2", role.Paths[0])
}

// Applying a delegation whose parent doesn't exist fails.
func TestApplyTargetsDelegationParentDoesntExist(t *testing.T) {
	_, repo, cs := testutils.EmptyRepo()

	// make sure a key exists for the previous level, so it's not a missing
	// key error, but we don't care about this key
	_, err := cs.Create("targets/level1", data.ED25519Key)
	assert.NoError(t, err)

	newKey, err := cs.Create("targets/level1/level2", data.ED25519Key)
	assert.NoError(t, err)

	// create delegation
	kl := data.KeyList{newKey}
	td := &changelist.TufDelegation{
		NewThreshold: 1,
		AddKeys:      kl,
	}

	tdJSON, err := json.Marshal(td)
	assert.NoError(t, err)

	ch := changelist.NewTufChange(
		changelist.ActionCreate,
		"targets/level1/level2",
		changelist.TypeTargetsDelegation,
		"",
		tdJSON,
	)

	err = applyTargetsChange(repo, ch)
	assert.Error(t, err)
	assert.IsType(t, data.ErrInvalidRole{}, err)
}
