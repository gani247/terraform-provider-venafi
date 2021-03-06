/*
 * Copyright 2018 Venafi, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cloud

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"github.com/Venafi/vcert/pkg/certificate"
	"github.com/Venafi/vcert/pkg/endpoint"
	"net/http"
	"strings"
	"time"
)

const apiURL = "api.venafi.cloud/v1/"

type urlResource string

const (
	urlResourceUserAccounts           urlResource = "useraccounts"
	urlResourcePing                               = "ping"
	urlResourceZones                              = "zones"
	urlResourceZoneByTag                          = urlResourceZones + "/tag/%s"
	urlResourceCertificatePolicies                = "certificatepolicies"
	urlResourcePoliciesByID                       = urlResourceCertificatePolicies + "/%s"
	urlResourcePoliciesForZoneByID                = urlResourceCertificatePolicies + "?zoneId=%s"
	urlResourceCertificateRequests                = "certificaterequests"
	urlResourceCertificateStatus                  = urlResourceCertificateRequests + "/%s"
	urlResourceCertificateRetrieve                = urlResourceCertificateRequests + "/%s/certificate"
	urlResourceCertificateSearch                  = "certificatesearch"
	urlResourceManagedCertificates                = "managedcertificates"
	urlResourceManagedCertificateById             = urlResourceManagedCertificates + "/%s"
)

type condorChainOption string

const (
	condorChainOptionRootFirst condorChainOption = "ROOT_FIRST"
	condorChainOptionRootLast                    = "EE_FIRST"
)

// Connector contains the base data needed to communicate with the Venafi Cloud servers
type Connector struct {
	baseURL string
	apiKey  string
	verbose bool
	user    *userDetails
	trust   *x509.CertPool
	zone    string
}

// NewConnector creates a new Venafi Cloud Connector object used to communicate with Venafi Cloud
func NewConnector(verbose bool, trust *x509.CertPool) *Connector {
	c := Connector{verbose: verbose, trust: trust}
	c.SetBaseURL(apiURL)
	return &c
}

func (c *Connector) SetZone(z string) {
	c.zone = z
}

func (c *Connector) GetType() endpoint.ConnectorType {
	return endpoint.ConnectorTypeCloud
}

//Ping attempts to connect to the Venafi Cloud API and returns an errror if it cannot
func (c *Connector) Ping() (err error) {
	url := c.getURL(urlResourcePing)

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("Unexpected status code on Venafi Cloud ping. Status: %d %s", resp.StatusCode, resp.Status)
	}
	return err
}

//Authenticate authenticates the user with Venafi Cloud using the provided API Key
func (c *Connector) Authenticate(auth *endpoint.Authentication) (err error) {
	if auth == nil {
		return fmt.Errorf("failed to authenticate: missing credentials")
	}
	c.apiKey = auth.APIKey
	url := c.getURL(urlResourceUserAccounts)
	statusCode, status, body, err := c.request("GET", url, nil, true)
	ud, err := parseUserDetailsResult(http.StatusOK, statusCode, status, body)
	if err != nil {
		return
	}
	c.user = ud
	return
}

//Register registers a new user with Venafi Cloud
func (c *Connector) Register(email string) (err error) {

	url := c.getURL(urlResourceUserAccounts)
	statusCode, status, body, err := c.request("POST", url, userAccount{Username: email, UserAccountType: "API"})

	//the user has already been registered and there is nothing to parse
	if statusCode == http.StatusAccepted {
		return nil
	}
	ud, err := parseUserDetailsResult(http.StatusCreated, statusCode, status, body)
	if err != nil {
		return err
	}
	c.user = ud
	return nil
}

func (c *Connector) ReadPolicyConfiguration(zone string) (policy *endpoint.Policy, err error) {
	config, err := c.ReadZoneConfiguration(zone)
	if err != nil {
		return nil, err
	}
	policy = &config.Policy
	return
}

//ReadZoneConfiguration reads the Zone information needed for generating and requesting a certificate from Venafi Cloud
func (c *Connector) ReadZoneConfiguration(zone string) (config *endpoint.ZoneConfiguration, err error) {
	z, err := c.getZoneByTag(zone)
	if err != nil {
		return nil, err
	}
	p, err := c.getPoliciesByID([]string{z.DefaultCertificateIdentityPolicy, z.DefaultCertificateUsePolicy})
	if err != nil {
		return
	}
	config = z.getZoneConfiguration(c.user, p)
	return config, nil
}

//RequestCertificate submits the CSR to the Venafi Cloud API for processing
func (c *Connector) RequestCertificate(req *certificate.Request, zone string) (requestID string, err error) {

	if zone == "" {
		zone = c.zone
	}

	if req.CsrOrigin == certificate.ServiceGeneratedCSR {
		return "", fmt.Errorf("service generated CSR is not supported by Saas service")
	}

	url := c.getURL(urlResourceCertificateRequests)
	if c.user == nil || c.user.Company == nil {
		return "", fmt.Errorf("Must be autheticated to request a certificate")
	}
	z, err := c.getZoneByTag(zone)
	if err != nil {
		return "", err
	}

	statusCode, status, body, err := c.request("POST", url, certificateRequest{ZoneID: z.ID, CSR: string(req.CSR)})

	if err != nil {
		return "", err
	}
	cr, err := parseCertificateRequestResult(statusCode, status, body)
	if err != nil {
		return "", err
	}
	requestID = cr.CertificateRequests[0].ID
	req.PickupID = requestID
	return requestID, nil
}

func (c *Connector) getCertificateStatus(requestID string) (certStatus *certificateStatus, err error) {
	url := c.getURL(urlResourceCertificateStatus)
	url = fmt.Sprintf(url, requestID)
	statusCode, _, body, err := c.request("GET", url, nil)

	if statusCode == http.StatusOK {
		certStatus = &certificateStatus{}
		err = json.Unmarshal(body, certStatus)
		if err != nil {
			return nil, fmt.Errorf("failed to parse certificate request status response: %s", err)
		}
		return
	}
	respErrors, err := parseResponseErrors(body)
	if err == nil {
		respError := fmt.Sprintf("Unexpected status code on Venafi Cloud certificate search. Status: %d\n", statusCode)
		for _, e := range respErrors {
			respError += fmt.Sprintf("Error Code: %d Error: %s\n", e.Code, e.Message)
		}
		return nil, fmt.Errorf(respError)
	}

	return nil, fmt.Errorf("Unexpected status code on Venafi Cloud certificate search. Status: %d", statusCode)

}

//RetrieveCertificate retrieves the certificate for the specified ID
func (c *Connector) RetrieveCertificate(req *certificate.Request) (certificates *certificate.PEMCollection, err error) {

	if req.FetchPrivateKey {
		return nil, fmt.Errorf("Failed to retrieve private key from Venafi Cloud service: not supported")
	}

	if req.PickupID == "" && req.Thumbprint != "" {
		// search cert by Thumbprint and fill pickupID
		var certificateRequestId string
		searchResult, err := c.searchCertificatesByFingerprint(req.Thumbprint)
		if err != nil {
			return nil, fmt.Errorf("Failed to retrieve certificate: %s", err)
		}
		if len(searchResult.Certificates) == 0 {
			return nil, fmt.Errorf("No certifiate found using fingerprint %s", req.Thumbprint)
		}

		reqIds := []string{}
		isOnlyOneCertificateRequestId := true
		for _, c := range searchResult.Certificates {
			reqIds = append(reqIds, c.CertificateRequestId)
			if certificateRequestId != "" && certificateRequestId != c.CertificateRequestId {
				isOnlyOneCertificateRequestId = false
			}
			certificateRequestId = c.CertificateRequestId
		}
		if !isOnlyOneCertificateRequestId {
			return nil, fmt.Errorf("More than one CertificateRequestId was found with the same Fingerprint: %s", reqIds)
		}

		req.PickupID = certificateRequestId
	}

	startTime := time.Now()
	for {
		status, err := c.getCertificateStatus(req.PickupID)
		if err != nil {
			return nil, fmt.Errorf("unable to retrieve: %s", err)
		}
		if status.Status == "ISSUED" {
			break // to fetch the cert itself
		} else if status.Status == "FAILED" {
			return nil, fmt.Errorf("Failed to retrieve certificate. Status: %v", status)
		}
		// status.Status == "REQUESTED" || status.Status == "PENDING"
		if req.Timeout == 0 {
			return nil, endpoint.ErrCertificatePending{CertificateID: req.PickupID, Status: status.Status}
		}
		if time.Now().After(startTime.Add(req.Timeout)) {
			return nil, endpoint.ErrRetrieveCertificateTimeout{CertificateID: req.PickupID}
		}
		// fmt.Printf("pending... %s\n", status.Status)
		time.Sleep(2 * time.Second)
	}

	url := c.getURL(urlResourceCertificateRetrieve)
	if c.user == nil || c.user.Company == nil {
		return nil, fmt.Errorf("Must be autheticated to retieve certificate")
	}
	url = fmt.Sprintf(url, req.PickupID)
	url += "?chainOrder=%s&format=PEM"
	switch req.ChainOption {
	case certificate.ChainOptionRootFirst:
		url = fmt.Sprintf(url, condorChainOptionRootFirst)
	default:
		url = fmt.Sprintf(url, condorChainOptionRootLast)
	}
	statusCode, status, body, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusOK {
		return newPEMCollectionFromResponse(body, req.ChainOption)
	} else if statusCode == http.StatusConflict { // Http Status Code 409 means the certificate has not been signed by the ca yet.
		return nil, endpoint.ErrCertificatePending{CertificateID: req.PickupID}
	} else {
		return nil, fmt.Errorf("Failed to retrieve certificate. StatusCode: %d -- Status: %s -- Server Data: %s", statusCode, status, body) //todo:remove body from err
	}
}

// RevokeCertificate attempts to revoke the certificate
func (c *Connector) RevokeCertificate(revReq *certificate.RevocationRequest) (err error) {
	return fmt.Errorf("not supported by endpoint")
}

// RenewCertificate attempts to renew the certificate
func (c *Connector) RenewCertificate(renewReq *certificate.RenewalRequest) (requestID string, err error) {

	/* 1st step is to get CertificateRequestId which is required to lookup managedCertificateId and zoneId */
	var certificateRequestId string

	if renewReq.Thumbprint != "" {
		// by Thumbprint (aka Fingerprint)
		searchResult, err := c.searchCertificatesByFingerprint(renewReq.Thumbprint)
		if err != nil {
			return "", fmt.Errorf("Failed to create renewal request: %s", err)
		}
		if len(searchResult.Certificates) == 0 {
			return "", fmt.Errorf("No certifiate found using fingerprint %s", renewReq.Thumbprint)
		}

		reqIds := []string{}
		isOnlyOneCertificateRequestId := true
		for _, c := range searchResult.Certificates {
			reqIds = append(reqIds, c.CertificateRequestId)
			if certificateRequestId != "" && certificateRequestId != c.CertificateRequestId {
				isOnlyOneCertificateRequestId = false
			}
			certificateRequestId = c.CertificateRequestId
		}
		if !isOnlyOneCertificateRequestId {
			return "", fmt.Errorf("Error: more than one CertificateRequestId was found with the same Fingerprint: %s", reqIds)
		}
	} else if renewReq.CertificateDN != "" {
		// by CertificateDN (which is the same as CertificateRequestId for current implementation)
		certificateRequestId = renewReq.CertificateDN
	} else {
		return "", fmt.Errorf("failed to create renewal request: CertificateDN or Thumbprint required")
	}

	/* 2nd step is to get ManagedCertificateId & ZoneId by looking up certificate request record */
	previousRequest, err := c.getCertificateStatus(certificateRequestId)
	if err != nil {
		return "", fmt.Errorf("certificate renew failed: %s", err)
	}
	var zoneId = previousRequest.ZoneId
	var managedCertificateId = previousRequest.ManagedCertificateId

	if managedCertificateId == "" {
		return "", fmt.Errorf("failed to submit renewal request for certificate: ManagedCertificateId is empty, certificate status is %s", previousRequest.Status)
	}

	if zoneId == "" {
		return "", fmt.Errorf("failed to submit renewal request for certificate: ZoneId is empty, certificate status is %s", previousRequest.Status)
	}

	/* 3rd step is to get ManagedCertificate Object by id
	   and check if latestCertificateRequestId there equals to certificateRequestId from 1st step */
	managedCertificate, err := c.getManagedCertificate(managedCertificateId)
	if err != nil {
		return "", fmt.Errorf("failed to renew certificate: %s", err)
	}
	if managedCertificate.LatestCertificateRequestId != certificateRequestId {
		withThumbprint := ""
		if renewReq.Thumbprint != "" {
			withThumbprint = fmt.Sprintf("with thumbprint %s ", renewReq.Thumbprint)
		}
		return "", fmt.Errorf(
			"Certificate under requestId %s "+withThumbprint+
				"is not the latest under ManagedCertificateId %s. The latest request is %s. "+
				"This error may happen when revoked certificate is requested to be renewed.",
			certificateRequestId, managedCertificateId, managedCertificate.LatestCertificateRequestId)
	}

	/* 4th step is to send renewal request */
	url := c.getURL(urlResourceCertificateRequests)
	if c.user == nil || c.user.Company == nil {
		return "", fmt.Errorf("Must be autheticated to request a certificate")
	}

	req := certificateRequest{ZoneID: zoneId, ExistingManagedCertificateId: managedCertificateId}
	if renewReq.CertificateRequest != nil && 0 < len(renewReq.CertificateRequest.CSR) {
		req.CSR = string(renewReq.CertificateRequest.CSR)
		req.ReuseCSR = false
	} else {
		req.ReuseCSR = true
	}
	statusCode, status, body, err := c.request("POST", url, req)
	if err != nil {
		return
	}

	cr, err := parseCertificateRequestResult(statusCode, status, body)
	if err != nil {
		return "", fmt.Errorf("Failed to renew certificate: %s", err)
	}
	return cr.CertificateRequests[0].ID, nil
}

func (c *Connector) getZoneByTag(tag string) (*zone, error) {
	url := c.getURL(urlResourceZoneByTag)
	if c.user == nil {
		return nil, fmt.Errorf("Must be autheticated to read the zone configuration")
	}
	url = fmt.Sprintf(url, tag)
	statusCode, status, body, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	z, err := parseZoneConfigurationResult(statusCode, status, body)
	if err != nil {
		return nil, err
	}
	return z, nil
}

func (c *Connector) getPoliciesByID(ids []string) (*certificatePolicy, error) {
	policy := new(certificatePolicy)
	if c.user == nil {
		return nil, fmt.Errorf("Must be autheticated to read the zone configuration")
	}
	for _, id := range ids {
		url := c.getURL(urlResourcePoliciesByID)
		url = fmt.Sprintf(url, id)
		statusCode, status, body, err := c.request("GET", url, nil)
		p, err := parseCertificatePolicyResult(statusCode, status, body)
		if err != nil {
			return nil, err
		}
		switch p.CertificatePolicyType {
		case certificatePolicyTypeIdentity:
			policy.SubjectCNRegexes = p.SubjectCNRegexes
			policy.SubjectORegexes = p.SubjectORegexes
			policy.SubjectOURegexes = p.SubjectOURegexes
			policy.SubjectSTRegexes = p.SubjectSTRegexes
			policy.SubjectLRegexes = p.SubjectLRegexes
			policy.SubjectCRegexes = p.SubjectCRegexes
			policy.SANRegexes = p.SANRegexes
		case certificatePolicyTypeUse:
			policy.KeyTypes = p.KeyTypes
			policy.KeyReuse = p.KeyReuse
		}
	}
	return policy, nil
}

func (c *Connector) searchCertificates(req *SearchRequest) (*CertificateSearchResponse, error) {

	var err error

	url := c.getURL(urlResourceCertificateSearch)
	statusCode, _, body, err := c.request("POST", url, req)

	searchResult, err := ParseCertificateSearchResponse(statusCode, body)
	if err != nil {
		return nil, err
	}
	return searchResult, nil
}

func (c *Connector) searchCertificatesByFingerprint(fp string) (*CertificateSearchResponse, error) {
	fp = strings.Replace(fp, ":", "", -1)
	fp = strings.Replace(fp, ".", "", -1)
	fp = strings.ToUpper(fp)
	req := &SearchRequest{
		Expression: &Expression{
			Operands: []Operand{
				{
					"fingerprint",
					MATCH,
					fp,
				},
			},
		},
	}
	return c.searchCertificates(req)
}

/*
  "id": "32a656d1-69b1-11e8-93d8-71014a32ec53",
  "companyId": "b5ed6d60-22c4-11e7-ac27-035f0608fd2c",
  "latestCertificateRequestId": "0e546560-69b1-11e8-9102-a1f1c55d36fb",
  "ownerUserId": "593cdba0-2124-11e8-8219-0932652c1da0",
  "certificateIds": [
    "32a656d0-69b1-11e8-93d8-71014a32ec53"
  ],
  "certificateName": "cn=svc6.venafi.example.com",

*/
type managedCertificate struct {
	Id                         string `json:"id"`
	CompanyId                  string `json:"companyId"`
	LatestCertificateRequestId string `json:"latestCertificateRequestId"`
	CertificateName            string `json:"certificateName"`
}

func (c *Connector) getManagedCertificate(managedCertId string) (*managedCertificate, error) {
	var err error
	url := c.getURL(urlResourceManagedCertificateById)
	url = fmt.Sprintf(url, managedCertId)
	statusCode, _, body, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}

	switch statusCode {
	case http.StatusOK:
		var res = &managedCertificate{}
		err = json.Unmarshal(body, res)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse search results: %s, body: %s", err, body)
		}
		return res, nil
	default:
		if body != nil {
			respErrors, err := parseResponseErrors(body)
			if err == nil {
				respError := fmt.Sprintf("Unexpected status code on Venafi Cloud certificate search. Status: %d\n", statusCode)
				for _, e := range respErrors {
					respError += fmt.Sprintf("Error Code: %d Error: %s\n", e.Code, e.Message)
				}
				return nil, fmt.Errorf(respError)
			}
		}
		return nil, fmt.Errorf("Unexpected status code on Venafi Cloud certificate search. Status: %d", statusCode)
	}

}

func (c *Connector) ImportCertificate(req *certificate.ImportRequest) (*certificate.ImportResponse, error) {
	return nil, fmt.Errorf("import is not supported")
}
