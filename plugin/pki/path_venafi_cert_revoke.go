package pki

import (
	"context"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/logical"
)

func pathVenafiCertRevoke(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "revoke/" + framework.GenericNameRegex("role"),
		Fields: map[string]*framework.FieldSchema{
			"role": {
				Type:        framework.TypeString,
				Description: `The desired role with configuration for this request`,
			},
			"certificate_uid": {
				Type:        framework.TypeString,
				Description: "Common name for created certificate",
			},
		},
		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.venafiCertRevoke,
		},

		HelpSynopsis:    pathConfigRootHelpSyn,
		HelpDescription: pathConfigRootHelpDesc,
	}
}

func (b *backend) venafiCertRevoke(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	return nil, nil
}
