package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/driver"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/stretchr/testify/assert"
)

// fakeCORSHandler satisfies driver.Handler (via the embedded nil interface, whose
// methods are never invoked) and driver.CORSManager (via the CORS method).
type fakeCORSHandler struct {
	driver.Handler
	corsErr error
	called  *bool
}

func (f *fakeCORSHandler) CORS() error {
	if f.called != nil {
		*f.called = true
	}
	return f.corsErr
}

// fakeNonCORSHandler satisfies driver.Handler but NOT driver.CORSManager,
// exercising the defensive branch where an allow-listed type's driver does not
// actually implement CORS.
type fakeNonCORSHandler struct {
	driver.Handler
}

// fakeDriverGetter records invocations so tests can assert whether a driver was
// constructed and with which policy.
type fakeDriverGetter struct {
	handler driver.Handler
	err     error
	calls   int
	policy  *ent.StoragePolicy
}

func (g *fakeDriverGetter) GetStorageDriver(ctx context.Context, policy *ent.StoragePolicy) (driver.Handler, error) {
	g.calls++
	g.policy = policy
	return g.handler, g.err
}

func asAppError(t *testing.T, err error) serializer.AppError {
	t.Helper()
	var appErr serializer.AppError
	assert.ErrorAs(t, err, &appErr)
	return appErr
}

// Every CORS-capable provider must pass the type gate, construct its driver and
// invoke CORS exactly once.
func TestEnableCORS_SupportedProvidersSucceed(t *testing.T) {
	a := assert.New(t)

	for _, pt := range []string{
		types.PolicyTypeOss,
		types.PolicyTypeCos,
		types.PolicyTypeS3,
		types.PolicyTypeKs3,
		types.PolicyTypeObs,
	} {
		called := false
		g := &fakeDriverGetter{handler: &fakeCORSHandler{called: &called}}

		err := enableCORS(context.Background(), g, &ent.StoragePolicy{Type: pt})

		a.NoError(err, "type %q should succeed", pt)
		a.Equal(1, g.calls, "type %q should construct the driver once", pt)
		a.True(called, "type %q should invoke CORS", pt)
	}
}

// Unsupported / unknown types must be rejected before any driver is constructed,
// preserving the original switch's behavior of never touching those drivers.
func TestEnableCORS_UnsupportedTypesRejected(t *testing.T) {
	a := assert.New(t)

	for _, pt := range []string{
		types.PolicyTypeLocal,
		types.PolicyTypeRemote,
		types.PolicyTypeQiniu,
		types.PolicyTypeUpyun,
		types.PolicyTypeOd,
		"bogus",
		"",
	} {
		g := &fakeDriverGetter{handler: &fakeCORSHandler{}}

		err := enableCORS(context.Background(), g, &ent.StoragePolicy{Type: pt})

		appErr := asAppError(t, err)
		a.Equal(serializer.CodeParamErr, appErr.Code, "type %q code", pt)
		a.Equal("Unsupported policy type", appErr.Msg, "type %q message", pt)
		a.Equal(0, g.calls, "type %q must not construct a driver", pt)
	}
}

// A driver construction failure keeps the CodeDBError code and the per-type
// "Failed to create <type> driver" message.
func TestEnableCORS_DriverCreationError(t *testing.T) {
	a := assert.New(t)

	cases := map[string]string{
		types.PolicyTypeOss: "Failed to create oss driver",
		types.PolicyTypeCos: "Failed to create cos driver",
		types.PolicyTypeS3:  "Failed to create s3 driver",
		types.PolicyTypeKs3: "Failed to create ks3 driver",
		types.PolicyTypeObs: "Failed to create obs driver",
	}

	for pt, wantMsg := range cases {
		raw := errors.New("construction failed")
		g := &fakeDriverGetter{err: raw}

		err := enableCORS(context.Background(), g, &ent.StoragePolicy{Type: pt})

		appErr := asAppError(t, err)
		a.Equal(serializer.CodeDBError, appErr.Code, "type %q code", pt)
		a.Equal(wantMsg, appErr.Msg, "type %q message", pt)
		a.Equal(raw, appErr.RawError, "type %q should wrap the original error", pt)
	}
}

// A CORS provisioning failure keeps the CodeInternalSetting code and the
// "Failed to create cors: <err>" message.
func TestEnableCORS_CORSError(t *testing.T) {
	a := assert.New(t)

	raw := errors.New("access denied")
	g := &fakeDriverGetter{handler: &fakeCORSHandler{corsErr: raw}}

	err := enableCORS(context.Background(), g, &ent.StoragePolicy{Type: types.PolicyTypeCos})

	appErr := asAppError(t, err)
	a.Equal(serializer.CodeInternalSetting, appErr.Code)
	a.Equal("Failed to create cors: access denied", appErr.Msg)
	a.Equal(raw, appErr.RawError)
}

// Defensive: an allow-listed type whose driver does not implement CORSManager is
// reported as unsupported rather than panicking on the type assertion.
func TestEnableCORS_DriverWithoutCORSSupport(t *testing.T) {
	a := assert.New(t)

	g := &fakeDriverGetter{handler: &fakeNonCORSHandler{}}

	err := enableCORS(context.Background(), g, &ent.StoragePolicy{Type: types.PolicyTypeOss})

	appErr := asAppError(t, err)
	a.Equal(serializer.CodeParamErr, appErr.Code)
	a.Equal("Unsupported policy type", appErr.Msg)
}

// The policy passed through to the factory must be the one handed to enableCORS.
func TestEnableCORS_PassesPolicyThrough(t *testing.T) {
	a := assert.New(t)

	called := false
	g := &fakeDriverGetter{handler: &fakeCORSHandler{called: &called}}
	policy := &ent.StoragePolicy{ID: 42, Type: types.PolicyTypeS3}

	err := enableCORS(context.Background(), g, policy)

	a.NoError(err)
	a.Same(policy, g.policy)
}
