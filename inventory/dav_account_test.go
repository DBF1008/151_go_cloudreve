package inventory

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/enttest"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/boolset"
	"github.com/cloudreve/Cloudreve/v4/pkg/conf"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupDavTest spins up an in-memory database (sqlite3 driver is registered in
// client.go's init) and creates a group, an owner user and a single dav account
// with read-only + proxy options set, returning the handles the tests need.
func setupDavTest(t *testing.T) (*ent.Client, DavAccountClient, *ent.User, *ent.DavAccount) {
	t.Helper()

	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()))
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	hasher, err := hashid.New("test_salt")
	require.NoError(t, err)
	davClient := NewDavAccountClient(client, conf.SQLiteDB, hasher)

	group, err := client.Group.Create().
		SetName("test-group").
		SetPermissions(&boolset.BooleanSet{}).
		Save(ctx)
	require.NoError(t, err)

	owner, err := client.User.Create().
		SetEmail("owner@example.com").
		SetNick("owner").
		SetGroupID(group.ID).
		Save(ctx)
	require.NoError(t, err)

	options := &boolset.BooleanSet{}
	boolset.Set(types.DavAccountReadOnly, true, options)
	boolset.Set(types.DavAccountProxy, true, options)

	account, err := davClient.Create(ctx, &CreateDavAccountParams{
		UserID:   owner.ID,
		Name:     "my-mount",
		URI:      "cloudreve://my/",
		Password: "originalpassword0000000000000000",
		Options:  options,
	})
	require.NoError(t, err)

	return client, davClient, owner, account
}

// TestDavAccountClient_UpdatePassword verifies that rotating the password sets a
// new password (and persists it) while leaving the URI, name, options and other
// metadata of the account untouched.
func TestDavAccountClient_UpdatePassword(t *testing.T) {
	_, davClient, _, account := setupDavTest(t)
	ctx := context.Background()
	a := assert.New(t)

	const newPwd = "rotatedpassword11111111111111111"
	updated, err := davClient.UpdatePassword(ctx, account.ID, newPwd)
	require.NoError(t, err)

	// Password is rotated and returned to the caller.
	a.Equal(newPwd, updated.Password)
	a.NotEqual(account.Password, updated.Password)

	// URI, name, owner, options and creation time are all preserved.
	a.Equal(account.Name, updated.Name)
	a.Equal(account.URI, updated.URI)
	a.Equal(account.OwnerID, updated.OwnerID)
	a.Equal(account.CreatedAt, updated.CreatedAt)
	require.NotNil(t, updated.Options)
	a.True(updated.Options.Enabled(int(types.DavAccountReadOnly)))
	a.True(updated.Options.Enabled(int(types.DavAccountProxy)))

	// The new password is what is actually stored.
	reloaded, err := davClient.GetByIDAndUserID(ctx, account.ID, account.OwnerID)
	require.NoError(t, err)
	a.Equal(newPwd, reloaded.Password)
	a.Equal(account.Name, reloaded.Name)
	a.Equal(account.URI, reloaded.URI)
}

// TestDavAccountClient_UpdateDoesNotChangePassword guards the semantics of the
// existing update path: editing name/URI/options must never rotate the password.
// Rotation is intentionally a separate operation.
func TestDavAccountClient_UpdateDoesNotChangePassword(t *testing.T) {
	_, davClient, _, account := setupDavTest(t)
	ctx := context.Background()
	a := assert.New(t)

	updated, err := davClient.Update(ctx, account.ID, &CreateDavAccountParams{
		Name:    "renamed-mount",
		URI:     "cloudreve://my/sub",
		Options: &boolset.BooleanSet{},
	})
	require.NoError(t, err)

	// Name/URI/options are updated...
	a.Equal("renamed-mount", updated.Name)
	a.Equal("cloudreve://my/sub", updated.URI)
	a.False(updated.Options.Enabled(int(types.DavAccountReadOnly)))
	a.False(updated.Options.Enabled(int(types.DavAccountProxy)))

	// ...but the password is left exactly as it was.
	a.Equal(account.Password, updated.Password)
}

// TestDavAccountClient_RotateScopedToOwner verifies the ownership guard the
// rotate endpoint relies on: only the owner can resolve (and therefore rotate)
// an account; a different user gets a not-found error.
func TestDavAccountClient_RotateScopedToOwner(t *testing.T) {
	client, davClient, owner, account := setupDavTest(t)
	ctx := context.Background()
	a := assert.New(t)

	otherGroup, err := client.Group.Create().
		SetName("other-group").
		SetPermissions(&boolset.BooleanSet{}).
		Save(ctx)
	require.NoError(t, err)
	other, err := client.User.Create().
		SetEmail("intruder@example.com").
		SetNick("intruder").
		SetGroupID(otherGroup.ID).
		Save(ctx)
	require.NoError(t, err)

	// The owner can resolve the account.
	_, err = davClient.GetByIDAndUserID(ctx, account.ID, owner.ID)
	a.NoError(err)

	// A non-owner cannot, so the rotate service rejects them before touching it.
	_, err = davClient.GetByIDAndUserID(ctx, account.ID, other.ID)
	a.Error(err)
	a.True(ent.IsNotFound(err))
}
