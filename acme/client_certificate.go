package acme

import (
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/xenolf/lego/log"
)

// ObtainCertificateForCSR tries to obtain a certificate matching the CSR passed into it.
// The domains are inferred from the CommonName and SubjectAltNames, if any. The private key
// for this CSR is not required.
// If bundle is true, the []byte contains both the issuer certificate and
// your issued certificate as a bundle.
// This function will never return a partial certificate. If one domain in the list fails,
// the whole certificate will fail.
func (c *Client) ObtainCertificateForCSR(csr x509.CertificateRequest, bundle bool) (*CertificateResource, error) {
	// figure out what domains it concerns
	// start with the common name
	domains := []string{csr.Subject.CommonName}

	// loop over the SubjectAltName DNS names
DNSNames:
	for _, sanName := range csr.DNSNames {
		for _, existingName := range domains {
			if existingName == sanName {
				// duplicate; skip this name
				continue DNSNames
			}
		}

		// name is unique
		domains = append(domains, sanName)
	}

	if bundle {
		log.Infof("[%s] acme: Obtaining bundled SAN certificate given a CSR", strings.Join(domains, ", "))
	} else {
		log.Infof("[%s] acme: Obtaining SAN certificate given a CSR", strings.Join(domains, ", "))
	}

	order, err := c.createOrderForIdentifiers(domains)
	if err != nil {
		return nil, err
	}
	authz, err := c.getAuthzForOrder(order)
	if err != nil {
		// If any challenge fails, return. Do not generate partial SAN certificates.
		/*for _, auth := range authz {
			c.disableAuthz(auth)
		}*/
		return nil, err
	}

	err = c.solveChallengeForAuthz(authz)
	if err != nil {
		// If any challenge fails, return. Do not generate partial SAN certificates.
		return nil, err
	}

	log.Infof("[%s] acme: Validations succeeded; requesting certificates", strings.Join(domains, ", "))

	failures := make(ObtainError)
	cert, err := c.requestCertificateForCsr(order, bundle, csr.Raw, nil)
	if err != nil {
		for _, chln := range authz {
			failures[chln.Identifier.Value] = err
		}
	}

	if cert != nil {
		// Add the CSR to the certificate so that it can be used for renewals.
		cert.CSR = pemEncode(&csr)
	}

	// do not return an empty failures map, because
	// it would still be a non-nil error value
	if len(failures) > 0 {
		return cert, failures
	}
	return cert, nil
}

func (c *Client) requestCertificateForCsr(order orderResource, bundle bool, csr []byte, privateKeyPem []byte) (*CertificateResource, error) {
	commonName := order.Domains[0]

	csrString := base64.RawURLEncoding.EncodeToString(csr)
	var retOrder orderMessage
	_, err := c.jws.postJSON(order.Finalize, csrMessage{Csr: csrString}, &retOrder)
	if err != nil {
		return nil, err
	}

	if retOrder.Status == statusInvalid {
		return nil, err
	}

	certRes := CertificateResource{
		Domain:     commonName,
		CertURL:    retOrder.Certificate,
		PrivateKey: privateKeyPem,
	}

	if retOrder.Status == statusValid {
		// if the certificate is available right away, short cut!
		ok, err := c.checkCertResponse(retOrder, &certRes, bundle)
		if err != nil {
			return nil, err
		}

		if ok {
			return &certRes, nil
		}
	}

	stopTimer := time.NewTimer(30 * time.Second)
	defer stopTimer.Stop()
	retryTick := time.NewTicker(500 * time.Millisecond)
	defer retryTick.Stop()

	for {
		select {
		case <-stopTimer.C:
			return nil, errors.New("certificate polling timed out")
		case <-retryTick.C:
			_, err := getJSON(order.URL, &retOrder)
			if err != nil {
				return nil, err
			}

			done, err := c.checkCertResponse(retOrder, &certRes, bundle)
			if err != nil {
				return nil, err
			}
			if done {
				return &certRes, nil
			}
		}
	}
}

// ObtainCertificate tries to obtain a single certificate using all domains passed into it.
// The first domain in domains is used for the CommonName field of the certificate, all other
// domains are added using the Subject Alternate Names extension. A new private key is generated
// for every invocation of this function. If you do not want that you can supply your own private key
// in the privKey parameter. If this parameter is non-nil it will be used instead of generating a new one.
// If bundle is true, the []byte contains both the issuer certificate and
// your issued certificate as a bundle.
// This function will never return a partial certificate. If one domain in the list fails,
// the whole certificate will fail.
func (c *Client) ObtainCertificate(domains []string, bundle bool, privKey crypto.PrivateKey, mustStaple bool) (*CertificateResource, error) {
	if len(domains) == 0 {
		return nil, errors.New("no domains to obtain a certificate for")
	}

	if bundle {
		log.Infof("[%s] acme: Obtaining bundled SAN certificate", strings.Join(domains, ", "))
	} else {
		log.Infof("[%s] acme: Obtaining SAN certificate", strings.Join(domains, ", "))
	}

	order, err := c.createOrderForIdentifiers(domains)
	if err != nil {
		return nil, err
	}
	authz, err := c.getAuthzForOrder(order)
	if err != nil {
		// If any challenge fails, return. Do not generate partial SAN certificates.
		/*for _, auth := range authz {
			c.disableAuthz(auth)
		}*/
		return nil, err
	}

	err = c.solveChallengeForAuthz(authz)
	if err != nil {
		// If any challenge fails, return. Do not generate partial SAN certificates.
		return nil, err
	}

	log.Infof("[%s] acme: Validations succeeded; requesting certificates", strings.Join(domains, ", "))

	failures := make(ObtainError)
	cert, err := c.requestCertificateForOrder(order, bundle, privKey, mustStaple)
	if err != nil {
		for _, auth := range authz {
			failures[auth.Identifier.Value] = err
		}
	}

	// do not return an empty failures map, because
	// it would still be a non-nil error value
	if len(failures) > 0 {
		return cert, failures
	}
	return cert, nil
}

func (c *Client) requestCertificateForOrder(order orderResource, bundle bool, privKey crypto.PrivateKey, mustStaple bool) (*CertificateResource, error) {

	var err error
	if privKey == nil {
		privKey, err = generatePrivateKey(c.keyType)
		if err != nil {
			return nil, err
		}
	}

	// determine certificate name(s) based on the authorization resources
	commonName := order.Domains[0]

	// ACME draft Section 7.4 "Applying for Certificate Issuance"
	// https://tools.ietf.org/html/draft-ietf-acme-acme-12#section-7.4
	// says:
	//   Clients SHOULD NOT make any assumptions about the sort order of
	//   "identifiers" or "authorizations" elements in the returned order
	//   object.
	san := []string{commonName}
	for _, auth := range order.Identifiers {
		if auth.Value != commonName {
			san = append(san, auth.Value)
		}
	}

	// TODO: should the CSR be customizable?
	csr, err := generateCsr(privKey, commonName, san, mustStaple)
	if err != nil {
		return nil, err
	}

	return c.requestCertificateForCsr(order, bundle, csr, pemEncode(privKey))
}

// RevokeCertificate takes a PEM encoded certificate or bundle and tries to revoke it at the CA.
func (c *Client) RevokeCertificate(certificate []byte) error {
	certificates, err := parsePEMBundle(certificate)
	if err != nil {
		return err
	}

	x509Cert := certificates[0]
	if x509Cert.IsCA {
		return fmt.Errorf("Certificate bundle starts with a CA certificate")
	}

	encodedCert := base64.URLEncoding.EncodeToString(x509Cert.Raw)

	_, err = c.jws.postJSON(c.directory.RevokeCertURL, revokeCertMessage{Certificate: encodedCert}, nil)
	return err
}

// RenewCertificate takes a CertificateResource and tries to renew the certificate.
// If the renewal process succeeds, the new certificate will ge returned in a new CertResource.
// Please be aware that this function will return a new certificate in ANY case that is not an error.
// If the server does not provide us with a new cert on a GET request to the CertURL
// this function will start a new-cert flow where a new certificate gets generated.
// If bundle is true, the []byte contains both the issuer certificate and
// your issued certificate as a bundle.
// For private key reuse the PrivateKey property of the passed in CertificateResource should be non-nil.
func (c *Client) RenewCertificate(cert CertificateResource, bundle, mustStaple bool) (*CertificateResource, error) {
	// Input certificate is PEM encoded. Decode it here as we may need the decoded
	// cert later on in the renewal process. The input may be a bundle or a single certificate.
	certificates, err := parsePEMBundle(cert.Certificate)
	if err != nil {
		return nil, err
	}

	x509Cert := certificates[0]
	if x509Cert.IsCA {
		return nil, fmt.Errorf("[%s] Certificate bundle starts with a CA certificate", cert.Domain)
	}

	// This is just meant to be informal for the user.
	timeLeft := x509Cert.NotAfter.Sub(time.Now().UTC())
	log.Infof("[%s] acme: Trying renewal with %d hours remaining", cert.Domain, int(timeLeft.Hours()))

	// We always need to request a new certificate to renew.
	// Start by checking to see if the certificate was based off a CSR, and
	// use that if it's defined.
	if len(cert.CSR) > 0 {
		csr, errP := pemDecodeTox509CSR(cert.CSR)
		if errP != nil {
			return nil, errP
		}
		newCert, failures := c.ObtainCertificateForCSR(*csr, bundle)
		return newCert, failures
	}

	var privKey crypto.PrivateKey
	if cert.PrivateKey != nil {
		privKey, err = parsePEMPrivateKey(cert.PrivateKey)
		if err != nil {
			return nil, err
		}
	}

	var domains []string
	// check for SAN certificate
	if len(x509Cert.DNSNames) > 1 {
		domains = append(domains, x509Cert.Subject.CommonName)
		for _, sanDomain := range x509Cert.DNSNames {
			if sanDomain == x509Cert.Subject.CommonName {
				continue
			}
			domains = append(domains, sanDomain)
		}
	} else {
		domains = append(domains, x509Cert.Subject.CommonName)
	}

	newCert, err := c.ObtainCertificate(domains, bundle, privKey, mustStaple)
	return newCert, err
}

// checkCertResponse checks to see if the certificate is ready and a link is contained in the
// response. if so, loads it into certRes and returns true. If the cert
// is not yet ready, it returns false. The certRes input
// should already have the Domain (common name) field populated. If bundle is
// true, the certificate will be bundled with the issuer's cert.
func (c *Client) checkCertResponse(order orderMessage, certRes *CertificateResource, bundle bool) (bool, error) {
	switch order.Status {
	case statusValid:
		resp, err := httpGet(order.Certificate)
		if err != nil {
			return false, err
		}

		cert, err := ioutil.ReadAll(http.MaxBytesReader(nil, resp.Body, maxBodySize))
		if err != nil {
			return false, err
		}

		// The issuer certificate link may be supplied via an "up" link
		// in the response headers of a new certificate.  See
		// https://tools.ietf.org/html/draft-ietf-acme-acme-12#section-7.4.2
		links := parseLinks(resp.Header["Link"])
		if link, ok := links["up"]; ok {
			issuerCert, err := c.getIssuerCertificateFromLink(link)

			if err != nil {
				// If we fail to acquire the issuer cert, return the issued certificate - do not fail.
				log.Warnf("[%s] acme: Could not bundle issuer certificate: %v", certRes.Domain, err)
			} else {
				issuerCert = pemEncode(derCertificateBytes(issuerCert))

				// If bundle is true, we want to return a certificate bundle.
				// To do this, we append the issuer cert to the issued cert.
				if bundle {
					cert = append(cert, issuerCert...)
				}

				certRes.IssuerCertificate = issuerCert
			}
		} else {
			// Get issuerCert from bundled response from Let's Encrypt
			// See https://community.letsencrypt.org/t/acme-v2-no-up-link-in-response/64962
			_, rest := pem.Decode(cert)
			if rest != nil {
				certRes.IssuerCertificate = rest
			}
		}

		certRes.Certificate = cert
		certRes.CertURL = order.Certificate
		certRes.CertStableURL = order.Certificate
		log.Infof("[%s] Server responded with a certificate.", certRes.Domain)
		return true, nil

	case "processing":
		return false, nil
	case statusInvalid:
		return false, errors.New("order has invalid state: invalid")
	default:
		return false, nil
	}
}

// getIssuerCertificateFromLink requests the issuer certificate
func (c *Client) getIssuerCertificateFromLink(url string) ([]byte, error) {
	log.Infof("acme: Requesting issuer cert from %s", url)
	resp, err := httpGet(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	issuerBytes, err := ioutil.ReadAll(http.MaxBytesReader(nil, resp.Body, maxBodySize))
	if err != nil {
		return nil, err
	}

	_, err = x509.ParseCertificate(issuerBytes)
	if err != nil {
		return nil, err
	}

	return issuerBytes, err
}

func (c *Client) createOrderForIdentifiers(domains []string) (orderResource, error) {
	var identifiers []identifier
	for _, domain := range domains {
		identifiers = append(identifiers, identifier{Type: "dns", Value: domain})
	}

	order := orderMessage{
		Identifiers: identifiers,
	}

	var response orderMessage
	hdr, err := c.jws.postJSON(c.directory.NewOrderURL, order, &response)
	if err != nil {
		return orderResource{}, err
	}

	orderRes := orderResource{
		URL:          hdr.Get("Location"),
		Domains:      domains,
		orderMessage: response,
	}
	return orderRes, nil
}

func parseLinks(links []string) map[string]string {
	aBrkt := regexp.MustCompile("[<>]")
	slver := regexp.MustCompile("(.+) *= *\"(.+)\"")
	linkMap := make(map[string]string)

	for _, link := range links {

		link = aBrkt.ReplaceAllString(link, "")
		parts := strings.Split(link, ";")

		matches := slver.FindStringSubmatch(parts[1])
		if len(matches) > 0 {
			linkMap[matches[2]] = parts[0]
		}
	}

	return linkMap
}