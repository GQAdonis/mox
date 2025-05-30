package http

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"rsc.io/qr"

	"github.com/mjl-/mox/admin"
	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/smtp"
)

var (
	metricAutoconf = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mox_autoconf_request_total",
			Help: "Number of autoconf requests.",
		},
		[]string{"domain"},
	)
	metricAutodiscover = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mox_autodiscover_request_total",
			Help: "Number of autodiscover requests.",
		},
		[]string{"domain"},
	)
)

// Autoconfiguration/Autodiscovery:
//
//   - Thunderbird will request an "autoconfig" xml file.
//   - Microsoft tools will request an "autodiscovery" xml file.
//   - In my tests on an internal domain, iOS mail only talks to Apple servers, then
//     does not attempt autoconfiguration. Possibly due to them being private DNS
//     names. Apple software can be provisioned with "mobileconfig" profile files,
//     which users can download after logging in.
//
// DNS records seem optional, but autoconfig.<domain> and autodiscover.<domain>
// (both CNAME or A) are useful, and so is SRV _autodiscovery._tcp.<domain> 0 0 443
// autodiscover.<domain> (or just <hostname> directly).
//
// Autoconf/discovery only works with valid TLS certificates, not with self-signed
// certs. So use it on public endpoints with certs signed by common CA's, or run
// your own (internal) CA and import the CA cert on your devices.
//
// Also see https://roll.urown.net/server/mail/autoconfig.html

// Autoconfiguration for Mozilla Thunderbird.
// User should create a DNS record: autoconfig.<domain> (CNAME or A).
// See https://wiki.mozilla.org/Thunderbird:Autoconfiguration:ConfigFileFormat
func autoconfHandle(w http.ResponseWriter, r *http.Request) {
	log := pkglog.WithContext(r.Context())

	var addrDom string
	defer func() {
		metricAutoconf.WithLabelValues(addrDom).Inc()
	}()

	email := r.FormValue("emailaddress")
	log.Debug("autoconfig request", slog.String("email", email))
	var domain dns.Domain
	if email == "" {
		email = "%EMAILADDRESS%"
		// Declare this here rather than using := to avoid shadowing domain from
		// the outer scope.
		var err error
		domain, err = dns.ParseDomain(r.Host)
		if err != nil {
			http.Error(w, fmt.Sprintf("400 - bad request - invalid domain: %s", r.Host), http.StatusBadRequest)
			return
		}
		domain.ASCII = strings.TrimPrefix(domain.ASCII, "autoconfig.")
		domain.Unicode = strings.TrimPrefix(domain.Unicode, "autoconfig.")
	} else {
		addr, err := smtp.ParseAddress(email)
		if err != nil {
			http.Error(w, "400 - bad request - invalid parameter emailaddress", http.StatusBadRequest)
			return
		}
		domain = addr.Domain
	}

	socketType := func(tlsMode admin.TLSMode) (string, error) {
		switch tlsMode {
		case admin.TLSModeImmediate:
			return "SSL", nil
		case admin.TLSModeSTARTTLS:
			return "STARTTLS", nil
		case admin.TLSModeNone:
			return "plain", nil
		default:
			return "", fmt.Errorf("unknown tls mode %v", tlsMode)
		}
	}

	var imapTLS, submissionTLS string
	config, err := admin.ClientConfigDomain(domain)
	if err == nil {
		imapTLS, err = socketType(config.IMAP.TLSMode)
	}
	if err == nil {
		submissionTLS, err = socketType(config.Submission.TLSMode)
	}
	if err != nil {
		http.Error(w, "400 - bad request - "+err.Error(), http.StatusBadRequest)
		return
	}

	// Thunderbird doesn't seem to allow U-labels, always return ASCII names.
	var resp autoconfigResponse
	resp.Version = "1.1"
	resp.EmailProvider.ID = domain.ASCII
	resp.EmailProvider.Domain = domain.ASCII
	resp.EmailProvider.DisplayName = email
	resp.EmailProvider.DisplayShortName = domain.ASCII

	// todo: specify SCRAM-SHA-256 once thunderbird and autoconfig supports it. or perhaps that will fall under "password-encrypted" by then.
	// todo: let user configure they prefer or require tls client auth and specify "TLS-client-cert"

	incoming := incomingServer{
		"imap",
		config.IMAP.Host.ASCII,
		config.IMAP.Port,
		imapTLS,
		email,
		"password-encrypted",
	}
	resp.EmailProvider.IncomingServers = append(resp.EmailProvider.IncomingServers, incoming)
	if config.IMAP.EnabledOnHTTPS {
		tlsMode, _ := socketType(admin.TLSModeImmediate)
		incomingALPN := incomingServer{
			"imap",
			config.IMAP.Host.ASCII,
			443,
			tlsMode,
			email,
			"password-encrypted",
		}
		resp.EmailProvider.IncomingServers = append(resp.EmailProvider.IncomingServers, incomingALPN)
	}

	outgoing := outgoingServer{
		"smtp",
		config.Submission.Host.ASCII,
		config.Submission.Port,
		submissionTLS,
		email,
		"password-encrypted",
	}
	resp.EmailProvider.OutgoingServers = append(resp.EmailProvider.OutgoingServers, outgoing)
	if config.Submission.EnabledOnHTTPS {
		tlsMode, _ := socketType(admin.TLSModeImmediate)
		outgoingALPN := outgoingServer{
			"smtp",
			config.Submission.Host.ASCII,
			443,
			tlsMode,
			email,
			"password-encrypted",
		}
		resp.EmailProvider.OutgoingServers = append(resp.EmailProvider.OutgoingServers, outgoingALPN)
	}

	// todo: should we put the email address in the URL?
	resp.ClientConfigUpdate.URL = fmt.Sprintf("https://autoconfig.%s/mail/config-v1.1.xml", domain.ASCII)

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	enc := xml.NewEncoder(w)
	enc.Indent("", "\t")
	fmt.Fprint(w, xml.Header)
	err = enc.Encode(resp)
	log.Check(err, "write autoconfig xml response")
}

// Autodiscover from Microsoft, also used by Thunderbird.
// User should create a DNS record: _autodiscover._tcp.<domain> SRV 0 0 443 <hostname>
//
// In practice, autodiscover does not seem to work wit microsoft clients. A
// connectivity test tool for outlook is available on
// https://testconnectivity.microsoft.com/, it has an option to do "Autodiscover to
// detect server settings". Incoming TLS connections are all failing, with various
// errors.
//
// Thunderbird does understand autodiscover.
func autodiscoverHandle(w http.ResponseWriter, r *http.Request) {
	log := pkglog.WithContext(r.Context())

	var addrDom string
	defer func() {
		metricAutodiscover.WithLabelValues(addrDom).Inc()
	}()

	if r.Method != "POST" {
		http.Error(w, "405 - method not allowed - post required", http.StatusMethodNotAllowed)
		return
	}

	var req autodiscoverRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "400 - bad request - parsing autodiscover request: "+err.Error(), http.StatusMethodNotAllowed)
		return
	}

	log.Debug("autodiscover request", slog.String("email", req.Request.EmailAddress))

	addr, err := smtp.ParseAddress(req.Request.EmailAddress)
	if err != nil {
		http.Error(w, "400 - bad request - invalid parameter emailaddress", http.StatusBadRequest)
		return
	}

	// tlsmode returns the "ssl" and "encryption" fields.
	tlsmode := func(tlsMode admin.TLSMode) (string, string, error) {
		switch tlsMode {
		case admin.TLSModeImmediate:
			return "on", "TLS", nil
		case admin.TLSModeSTARTTLS:
			return "on", "", nil
		case admin.TLSModeNone:
			return "off", "", nil
		default:
			return "", "", fmt.Errorf("unknown tls mode %v", tlsMode)
		}
	}

	var imapSSL, imapEncryption string
	var submissionSSL, submissionEncryption string
	config, err := admin.ClientConfigDomain(addr.Domain)
	if err == nil {
		imapSSL, imapEncryption, err = tlsmode(config.IMAP.TLSMode)
	}
	if err == nil {
		submissionSSL, submissionEncryption, err = tlsmode(config.Submission.TLSMode)
	}
	if err != nil {
		http.Error(w, "400 - bad request - "+err.Error(), http.StatusBadRequest)
		return
	}

	// The docs are generated and fragmented in many tiny pages, hard to follow.
	// High-level starting point, https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxdscli/78530279-d042-4eb0-a1f4-03b18143cd19
	// Request: https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxdscli/2096fab2-9c3c-40b9-b123-edf6e8d55a9b
	// Response, protocol: https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxdscli/f4238db6-a983-435c-807a-b4b4a624c65b
	// It appears autodiscover does not allow specifying SCRAM-SHA-256 as
	// authentication method, or any authentication method that real clients actually
	// use. See
	// https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxdscli/21fd2dd5-c4ee-485b-94fb-e7db5da93726

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")

	// todo: let user configure they prefer or require tls client auth and add "AuthPackage" with value "certificate" to Protocol? see https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxdscli/21fd2dd5-c4ee-485b-94fb-e7db5da93726

	resp := autodiscoverResponse{}
	resp.XMLName.Local = "Autodiscover"
	resp.XMLName.Space = "http://schemas.microsoft.com/exchange/autodiscover/responseschema/2006"
	resp.Response.XMLName.Local = "Response"
	resp.Response.XMLName.Space = "http://schemas.microsoft.com/exchange/autodiscover/outlook/responseschema/2006a"
	resp.Response.Account = autodiscoverAccount{
		AccountType: "email",
		Action:      "settings",
		Protocol: []autodiscoverProtocol{
			{
				Type:         "IMAP",
				Server:       config.IMAP.Host.ASCII,
				Port:         config.IMAP.Port,
				LoginName:    req.Request.EmailAddress,
				SSL:          imapSSL,
				Encryption:   imapEncryption,
				SPA:          "off", // Override default "on", this is Microsofts proprietary authentication protocol.
				AuthRequired: "on",
			},
			{
				Type:         "SMTP",
				Server:       config.Submission.Host.ASCII,
				Port:         config.Submission.Port,
				LoginName:    req.Request.EmailAddress,
				SSL:          submissionSSL,
				Encryption:   submissionEncryption,
				SPA:          "off", // Override default "on", this is Microsofts proprietary authentication protocol.
				AuthRequired: "on",
			},
		},
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "\t")
	fmt.Fprint(w, xml.Header)
	err = enc.Encode(resp)
	log.Check(err, "marshal autodiscover xml response")
}

// Thunderbird requests these URLs for autoconfig/autodiscover:
// https://autoconfig.example.org/mail/config-v1.1.xml?emailaddress=user%40example.org
// https://autodiscover.example.org/autodiscover/autodiscover.xml
// https://example.org/.well-known/autoconfig/mail/config-v1.1.xml?emailaddress=user%40example.org
// https://example.org/autodiscover/autodiscover.xml
type incomingServer struct {
	Type           string `xml:"type,attr"`
	Hostname       string `xml:"hostname"`
	Port           int    `xml:"port"`
	SocketType     string `xml:"socketType"`
	Username       string `xml:"username"`
	Authentication string `xml:"authentication"`
}
type outgoingServer struct {
	Type           string `xml:"type,attr"`
	Hostname       string `xml:"hostname"`
	Port           int    `xml:"port"`
	SocketType     string `xml:"socketType"`
	Username       string `xml:"username"`
	Authentication string `xml:"authentication"`
}
type autoconfigResponse struct {
	XMLName xml.Name `xml:"clientConfig"`
	Version string   `xml:"version,attr"`

	EmailProvider struct {
		ID               string `xml:"id,attr"`
		Domain           string `xml:"domain"`
		DisplayName      string `xml:"displayName"`
		DisplayShortName string `xml:"displayShortName"`

		IncomingServers []incomingServer `xml:"incomingServer"`
		OutgoingServers []outgoingServer `xml:"outgoingServer"`
	} `xml:"emailProvider"`

	ClientConfigUpdate struct {
		URL string `xml:"url,attr"`
	} `xml:"clientConfigUpdate"`
}

type autodiscoverRequest struct {
	XMLName xml.Name `xml:"Autodiscover"`
	Request struct {
		EmailAddress             string `xml:"EMailAddress"`
		AcceptableResponseSchema string `xml:"AcceptableResponseSchema"`
	}
}

type autodiscoverResponse struct {
	XMLName  xml.Name
	Response struct {
		XMLName xml.Name
		Account autodiscoverAccount
	}
}

type autodiscoverAccount struct {
	AccountType string
	Action      string
	Protocol    []autodiscoverProtocol
}

type autodiscoverProtocol struct {
	Type          string
	Server        string
	Port          int
	DirectoryPort int
	ReferralPort  int
	LoginName     string
	SSL           string
	Encryption    string `xml:",omitempty"`
	SPA           string
	AuthRequired  string
}

// Serve a .mobileconfig file. This endpoint is not a standard place where Apple
// devices look. We point to it from the account page.
func mobileconfigHandle(w http.ResponseWriter, r *http.Request) {
	log := pkglog.WithContext(r.Context())

	if r.Method != "GET" {
		http.Error(w, "405 - method not allowed - get required", http.StatusMethodNotAllowed)
		return
	}
	addresses := r.FormValue("addresses")
	fullName := r.FormValue("name")
	var buf []byte
	var err error
	if addresses == "" {
		err = fmt.Errorf("missing/empty field addresses")
	}
	l := strings.Split(addresses, ",")
	if err == nil {
		buf, err = MobileConfig(l, fullName)
	}
	if err != nil {
		http.Error(w, "400 - bad request - "+err.Error(), http.StatusBadRequest)
		return
	}
	h := w.Header()
	filename := l[0]
	filename = strings.ReplaceAll(filename, ".", "-")
	filename = strings.ReplaceAll(filename, "@", "-at-")
	filename = "email-account-" + filename + ".mobileconfig"
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	_, err = w.Write(buf)
	log.Check(err, "writing mobileconfig response")
}

// Serve a png file with qrcode with the link to the .mobileconfig file, should be
// helpful for mobile devices.
func mobileconfigQRCodeHandle(w http.ResponseWriter, r *http.Request) {
	log := pkglog.WithContext(r.Context())

	if r.Method != "GET" {
		http.Error(w, "405 - method not allowed - get required", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasSuffix(r.URL.Path, ".qrcode.png") {
		http.NotFound(w, r)
		return
	}

	// Compose URL, scheme and host are not set.
	u := *r.URL
	if r.TLS == nil {
		u.Scheme = "http"
	} else {
		u.Scheme = "https"
	}
	u.Host = r.Host
	u.Path = strings.TrimSuffix(u.Path, ".qrcode.png")

	code, err := qr.Encode(u.String(), qr.L)
	if err != nil {
		http.Error(w, "500 - internal server error - generating qr-code: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	_, err = w.Write(code.PNG())
	log.Check(err, "writing mobileconfig qr code")
}
