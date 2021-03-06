package pki

import (
	"encoding/base64"
	"fmt"

	"github.com/hashicorp/vault/helper/certutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

func pathGenerateIntermediate(b *backend) *framework.Path {
	ret := &framework.Path{
		Pattern: "intermediate/generate/" + framework.GenericNameRegex("exported"),

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.pathGenerateIntermediate,
		},

		HelpSynopsis:    pathGenerateIntermediateHelpSyn,
		HelpDescription: pathGenerateIntermediateHelpDesc,
	}

	ret.Fields = addCACommonFields(map[string]*framework.FieldSchema{})
	ret.Fields = addCAKeyGenerationFields(ret.Fields)

	return ret
}

func pathSetSignedIntermediate(b *backend) *framework.Path {
	ret := &framework.Path{
		Pattern: "intermediate/set-signed",

		Fields: map[string]*framework.FieldSchema{
			"certificate": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `PEM-format certificate. This must be a CA
certificate with a public key matching the
previously-generated key from the generation
endpoint.`,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.pathSetSignedIntermediate,
		},

		HelpSynopsis:    pathSetSignedIntermediateHelpSyn,
		HelpDescription: pathSetSignedIntermediateHelpDesc,
	}

	return ret
}

func (b *backend) pathGenerateIntermediate(
	req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	var err error

	exported, format, role, errorResp := b.getGenerationParams(data)
	if errorResp != nil {
		return errorResp, nil
	}

	var resp *logical.Response
	parsedBundle, err := generateIntermediateCSR(b, role, nil, req, data)
	if err != nil {
		switch err.(type) {
		case certutil.UserError:
			return logical.ErrorResponse(err.Error()), nil
		case certutil.InternalError:
			return nil, err
		}
	}

	csrb, err := parsedBundle.ToCSRBundle()
	if err != nil {
		return nil, fmt.Errorf("Error converting raw CSR bundle to CSR bundle: %s", err)
	}

	resp = &logical.Response{
		Data: map[string]interface{}{},
	}

	switch format {
	case "pem":
		resp.Data["csr"] = csrb.CSR
		if exported {
			resp.Data["private_key"] = csrb.PrivateKey
			resp.Data["private_key_type"] = csrb.PrivateKeyType
		}
	case "der":
		resp.Data["csr"] = base64.StdEncoding.EncodeToString(parsedBundle.CSRBytes)
		if exported {
			resp.Data["private_key"] = base64.StdEncoding.EncodeToString(parsedBundle.PrivateKeyBytes)
			resp.Data["private_key_type"] = csrb.PrivateKeyType
		}
	}

	cb := &certutil.CertBundle{}
	cb.PrivateKey = csrb.PrivateKey
	cb.PrivateKeyType = csrb.PrivateKeyType

	entry, err := logical.StorageEntryJSON("config/ca_bundle", cb)
	if err != nil {
		return nil, err
	}
	err = req.Storage.Put(entry)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (b *backend) pathSetSignedIntermediate(
	req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	cert := data.Get("certificate").(string)

	if cert == "" {
		return logical.ErrorResponse("no certificate provided in the \"certficate\" parameter"), nil
	}

	inputBundle, err := certutil.ParsePEMBundle(cert)
	if err != nil {
		switch err.(type) {
		case certutil.InternalError:
			return nil, err
		default:
			return logical.ErrorResponse(err.Error()), nil
		}
	}

	// If only one certificate is provided and it's a CA
	// the parsing will assign it to the IssuingCA, so move it over
	if inputBundle.Certificate == nil && inputBundle.IssuingCA != nil {
		inputBundle.Certificate = inputBundle.IssuingCA
		inputBundle.IssuingCA = nil
		inputBundle.CertificateBytes = inputBundle.IssuingCABytes
		inputBundle.IssuingCABytes = nil
	}

	if inputBundle.Certificate == nil {
		return logical.ErrorResponse("supplied certificate could not be successfully parsed"), nil
	}

	cb := &certutil.CertBundle{}
	entry, err := req.Storage.Get("config/ca_bundle")
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return logical.ErrorResponse("could not find any existing entry with a private key"), nil
	}

	err = entry.DecodeJSON(cb)
	if err != nil {
		return nil, err
	}

	if len(cb.PrivateKey) == 0 || cb.PrivateKeyType == "" {
		return logical.ErrorResponse("could not find an existing privat key"), nil
	}

	parsedCB, err := cb.ToParsedCertBundle()
	if err != nil {
		return nil, err
	}
	if parsedCB.PrivateKey == nil {
		return nil, fmt.Errorf("saved key could not be parsed successfully")
	}

	equal, err := certutil.ComparePublicKeys(parsedCB.PrivateKey.Public(), inputBundle.Certificate.PublicKey)
	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf(
			"error matching public keys: %v", err)), nil
	}
	if !equal {
		return logical.ErrorResponse("key in certificate does not match stored key"), nil
	}

	inputBundle.PrivateKey = parsedCB.PrivateKey
	inputBundle.PrivateKeyType = parsedCB.PrivateKeyType
	inputBundle.PrivateKeyBytes = parsedCB.PrivateKeyBytes

	if !inputBundle.Certificate.IsCA {
		return logical.ErrorResponse("the given certificate is not marked for CA use and cannot be used with this backend"), nil
	}

	cb, err = inputBundle.ToCertBundle()
	if err != nil {
		return nil, fmt.Errorf("error converting raw values into cert bundle: %s", err)
	}

	entry, err = logical.StorageEntryJSON("config/ca_bundle", cb)
	if err != nil {
		return nil, err
	}
	err = req.Storage.Put(entry)
	if err != nil {
		return nil, err
	}

	// For ease of later use, also store just the certificate at a known
	// location
	entry.Key = "ca"
	entry.Value = inputBundle.CertificateBytes
	err = req.Storage.Put(entry)
	if err != nil {
		return nil, err
	}

	// Build a fresh CRL
	err = buildCRL(b, req)

	return nil, err
}

const pathGenerateIntermediateHelpSyn = `
Generate a new CSR and private key used for signing.
`

const pathGenerateIntermediateHelpDesc = `
See the API documentation for more information.
`

const pathSetSignedIntermediateHelpSyn = `
Provide the signed intermediate CA cert.
`

const pathSetSignedIntermediateHelpDesc = `
See the API documentation for more information.
`
