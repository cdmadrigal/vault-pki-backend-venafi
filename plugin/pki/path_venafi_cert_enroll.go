package pki

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	vcertificate "github.com/Venafi/vcert/pkg/certificate"
	"github.com/Venafi/vcert/pkg/endpoint"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
	"log"
	"strings"
	"time"
)

func pathVenafiCertEnroll(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "issue/" + framework.GenericNameRegex("role"),
		Fields: map[string]*framework.FieldSchema{
			"role": {
				Type:        framework.TypeString,
				Description: `The desired role with configuration for this request`,
			},
			"common_name": {
				Type:        framework.TypeString,
				Description: "Common name for created certificate",
			},
			"alt_names": {
				Type:        framework.TypeCommaStringSlice,
				Description: "Alternative names for created certificate",
			},
		},
		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.pathVenafiCertObtain,
		},

		HelpSynopsis:    pathConfigRootHelpSyn,
		HelpDescription: pathConfigRootHelpDesc,
	}
}

func (b *backend) pathVenafiCertObtain(ctx context.Context, req *logical.Request, data *framework.FieldData) (
	*logical.Response, error) {

	log.Printf("Getting the role\n")
	roleName := data.Get("role").(string)

	role, err := b.getRole(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, fmt.Errorf("Unknown role +%v", role)
	}

	commonName := data.Get("common_name").(string)
	altNames := data.Get("alt_names").([]string)
	if len(commonName) == 0 && len(altNames) == 0 {
		return logical.ErrorResponse("no domains specified on certificate"), nil
	}
	if len(commonName) == 0 && len(altNames) > 0 {
		commonName = altNames[0]
	}

	log.Println("Signing certificate " + commonName)
	log.Printf("ALTNAMES is is %T %p %s", altNames, &altNames, altNames)
	log.Println("Running venafi client:")
	cl, err := b.ClientVenafi(ctx, req.Storage, data, req, roleName)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	var pk privateKey

	pk.keyType = role.KeyType
	pk.keyBits = role.KeyBits
	pk.keyCurve = role.KeyCurve
	log.Printf("Signing private key with parameteres %v", pk)

	certReq, pkey, err := createVenafiCSR(commonName, altNames, pk)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	log.Printf("Running enroll request")
	id, err := cl.RequestCertificate(certReq, "")
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	certReq.PickupID = id

	var cert *vcertificate.PEMCollection
	for {
		cert, err = cl.RetrieveCertificate(certReq)
		if err != nil {
			_, pending := err.(endpoint.ErrCertificatePending)
			_, timeout := err.(endpoint.ErrRetrieveCertificateTimeout)

			if pending || timeout {
				log.Printf("Certificate %s Issue pending with id %s", commonName, id)
				time.Sleep(5 * time.Second)
				continue
			} else {
				return logical.ErrorResponse(err.Error()), nil
			}
		}
		log.Printf("Certificate is %s", *cert)
		log.Printf("successfully got certificate: cn=%q altNames=%+v", commonName, altNames)
		break
	}

	certificate := strings.Join([]string{cert.Certificate}, "\n")
	cs := append([]string{cert.Certificate}, cert.Chain...)
	chain := strings.Join(cs, "\n")
	log.Println("certificate: ", chain)

	//Parsing certificate and getting it's serial number
	pemBlock, _ := pem.Decode([]byte(certificate))
	parsedCertificate, err := x509.ParseCertificate(pemBlock.Bytes)
	serialNumber := getHexFormatted(parsedCertificate.SerialNumber.Bytes(), ":")

	encoded_key := pem.EncodeToMemory(pkey)
	log.Println("Writing chain:", chain, "And key: ", string(encoded_key))

	var entry *logical.StorageEntry

	if role.StorePrivateKey {
		entry, err = logical.StorageEntryJSON("", VenafiCert{
			Certificate:      certificate,
			CertificateChain: chain,
			PrivateKey:       string(encoded_key),
			SerialNumber:     serialNumber,
		})
	} else {
		entry, err = logical.StorageEntryJSON("", VenafiCert{
			Certificate:      certificate,
			CertificateChain: chain,
			SerialNumber:     serialNumber,
		})
	}

	if role.StoreByCN {

		//Writing certificate to the storage with CN
		log.Println("Putting certificate to the certs/" + commonName)
		entry.Key = "certs/" + commonName

		if err := req.Storage.Put(ctx, entry); err != nil {
			log.Println("Error putting entry to storage")
			return nil, err
		}
	}

	if role.StoreBySerial {

		//Writing certificate to the storage with Serial Number
		log.Println("Putting certificate to the certs/", normalizeSerial(serialNumber))
		entry.Key = "certs/" + normalizeSerial(serialNumber)

		if err := req.Storage.Put(ctx, entry); err != nil {
			log.Println("Error putting entry to storage")
			return nil, err
		}
	}

	respData := map[string]interface{}{
		"common_name":       commonName,
		"serial_number":     serialNumber,
		"certificate_chain": chain,
		"certificate":       certificate,
		"private_key":       string(encoded_key),
	}

	var logResp *logical.Response
	switch {
	case role.GenerateLease == false:
		// If lease generation is disabled do not populate `Secret` field in
		// the response
		logResp = &logical.Response{
			Data: respData,
		}
	default:
		logResp = b.Secret(SecretCertsType).Response(
			respData,
			map[string]interface{}{
				"serial_number": serialNumber,
			})
		TTL := parsedCertificate.NotAfter.Sub(time.Now())
		log.Println("Seting up secret lease duration to: ", TTL)
		logResp.Secret.TTL = TTL
	}

	logResp.AddWarning("Read access to this endpoint should be controlled via ACLs as it will return the connection private key as it is.")
	return logResp, nil
}

type privateKey struct {
	keyBits  int
	keyCurve string
	keyType  string
}

func createVenafiCSR(commonName string, altNames []string, pk privateKey) (*vcertificate.Request, *pem.Block, error) {
	var err error
	const defaultKeySize = 2048
	req := &vcertificate.Request{}

	if len(commonName) == 0 && len(altNames) == 0 {
		return req, nil, fmt.Errorf("no domains specified on certificate")
	}
	if len(commonName) == 0 && len(altNames) > 0 {
		commonName = altNames[0]
	}

	//Obtain a certificate from the Venafi server
	log.Printf("Using CN %s and SAN %s", commonName, altNames)
	req.Subject.CommonName = commonName
	//Adding alt names if exists
	dnsnum := len(altNames)
	if dnsnum > 0 {
		req.DNSNames = make([]string, 0, dnsnum)
		for i := 0; i < dnsnum; i++ {
			val := altNames[i]
			log.Printf("Adding SAN %s.", val)
			req.DNSNames = append(req.DNSNames, val)
		}
	}
	//Appending common name to the DNS names if it is not there
	if !sliceContains(req.DNSNames, commonName) {
		log.Printf("Adding CN %s to SAN because it wasn't included.", commonName)
		req.DNSNames = append(req.DNSNames, commonName)
	}

	log.Printf("Requested SAN: %s", req.DNSNames)

	if pk.keyType == "rsa" {
		req.KeyLength = pk.keyBits
	} else if pk.keyType == "ec" {
		req.KeyType = vcertificate.KeyTypeECDSA
		switch {
		case pk.keyCurve == "P224":
			req.KeyCurve = vcertificate.EllipticCurveP224
		case pk.keyCurve == "P256":
			req.KeyCurve = vcertificate.EllipticCurveP256
		case pk.keyCurve == "P384":
			req.KeyCurve = vcertificate.EllipticCurveP384
		case pk.keyCurve == "P521":
			req.KeyCurve = vcertificate.EllipticCurveP521
		default:

		}

	} else {
		return req, nil, fmt.Errorf("can't determine key algorithm %s", pk.keyType)
	}

	switch req.KeyType {
	case vcertificate.KeyTypeECDSA:
		req.PrivateKey, err = vcertificate.GenerateECDSAPrivateKey(req.KeyCurve)
	case vcertificate.KeyTypeRSA:
		req.PrivateKey, err = vcertificate.GenerateRSAPrivateKey(req.KeyLength)
	default:
		log.Printf("Unable to generate certificate request, key type %s is not supported", req.KeyType.String())
		return nil, nil, err
	}

	key, err := getPrivateKeyPEMBock(req.PrivateKey)
	if err != nil {
		return nil, nil, err
	}

	//Setting up CSR
	certificateRequest := x509.CertificateRequest{}
	certificateRequest.Subject = req.Subject
	certificateRequest.DNSNames = req.DNSNames
	certificateRequest.EmailAddresses = req.EmailAddresses
	certificateRequest.IPAddresses = req.IPAddresses
	certificateRequest.Attributes = req.Attributes

	/* TODO:
	zoneConfig, err = cs.Conn.ReadZoneConfiguration(cf.Zone)
	zoneConfig.UpdateCertificateRequest(req)
		...should happen somewhere here before CSR is signed */

	csr, err := x509.CreateCertificateRequest(rand.Reader, &certificateRequest, req.PrivateKey)
	if err != nil {
		return nil, nil, err
	}

	req.CSR = csr

	req.CSR = pem.EncodeToMemory(vcertificate.GetCertificateRequestPEMBlock(req.CSR))

	return req, key, nil
}

type VenafiCert struct {
	Certificate      string `json:"certificate"`
	CertificateChain string `json:"certificate_chain"`
	PrivateKey       string `json:"private_key"`
	SerialNumber     string `json:"serial_number"`
}

const pathConfigRootHelpSyn = `
Configure the Venafi TPP credentials that are used to manage certificates,
`

const pathConfigRootHelpDesc = `
Configure TPP first
`
