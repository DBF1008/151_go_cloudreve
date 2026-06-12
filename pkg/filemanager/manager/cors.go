package manager

import (
	"context"
	"fmt"

	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/driver"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
)

// corsCapablePolicyTypes enumerates the storage policy types whose drivers
// support configuring CORS rules. Only these types are admitted into the CORS
// provisioning flow; every other type is rejected as unsupported. Keeping this
// set alongside the shared GetStorageDriver factory means adding a new CORS
// provider only requires registering its driver and listing it here.
var corsCapablePolicyTypes = map[string]struct{}{
	types.PolicyTypeOss: {},
	types.PolicyTypeCos: {},
	types.PolicyTypeS3:  {},
	types.PolicyTypeKs3: {},
	types.PolicyTypeObs: {},
}

// driverGetter creates a storage handler for a policy. It is satisfied by
// *manager and exists so enableCORS can be unit-tested without constructing a
// full manager or talking to real object storage backends.
type driverGetter interface {
	GetStorageDriver(ctx context.Context, policy *ent.StoragePolicy) (driver.Handler, error)
}

// enableCORS provisions the CORS rules required by Cloudreve on the storage
// backend of the given policy. It admits only policy types whose driver supports
// CORS, then reuses the shared storage driver factory to build the handler so
// driver initialization stays in a single place. Error semantics mirror the
// previous per-driver implementation:
//   - unsupported/unknown type: CodeParamErr "Unsupported policy type"
//   - driver construction failure: CodeDBError "Failed to create <type> driver"
//   - CORS provisioning failure: CodeInternalSetting "Failed to create cors: <err>"
func enableCORS(ctx context.Context, g driverGetter, policy *ent.StoragePolicy) error {
	if _, ok := corsCapablePolicyTypes[policy.Type]; !ok {
		return serializer.NewError(serializer.CodeParamErr, "Unsupported policy type", nil)
	}

	handler, err := g.GetStorageDriver(ctx, policy)
	if err != nil {
		return serializer.NewError(serializer.CodeDBError, fmt.Sprintf("Failed to create %s driver", policy.Type), err)
	}

	corsHandler, ok := handler.(driver.CORSManager)
	if !ok {
		// The allow-list claims this type is CORS-capable but its driver does not
		// implement the capability. Treat it as unsupported rather than panicking.
		return serializer.NewError(serializer.CodeParamErr, "Unsupported policy type", nil)
	}

	if err := corsHandler.CORS(); err != nil {
		return serializer.NewError(serializer.CodeInternalSetting, "Failed to create cors: "+err.Error(), err)
	}

	return nil
}

// EnableCORS provisions CORS rules on the storage backend of the given policy.
// Only policy types whose driver supports CORS are accepted; others return a
// CodeParamErr "Unsupported policy type" error.
func (m *manager) EnableCORS(ctx context.Context, policy *ent.StoragePolicy) error {
	return enableCORS(ctx, m, policy)
}
