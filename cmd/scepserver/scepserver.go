package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/micromdm/scep/v2/csrverifier"
	executablecsrverifier "github.com/micromdm/scep/v2/csrverifier/executable"
	scepdepot "github.com/micromdm/scep/v2/depot"
	"github.com/micromdm/scep/v2/depot/file"
	scepserver "github.com/micromdm/scep/v2/server"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"
)

// version info
var (
	version = "unknown"
)

func main() {
	var caCMD = flag.NewFlagSet("ca", flag.ExitOnError)
	{
		if len(os.Args) >= 2 {
			if os.Args[1] == "ca" {
				status := caMain(caCMD, os.Args[2:])
				os.Exit(status)
			}
		}
	}

	//main flags
	var (
		flVersion           = flag.Bool("version", envBool("SCEP_VERSION"), "prints version information")
		flHTTPAddr          = flag.String("http-addr", envString("SCEP_HTTP_ADDR", ""), "http listen address. defaults to \":8080\"")
		flPort              = flag.String("port", envString("SCEP_HTTP_LISTEN_PORT", "8080"), "http port to listen on (if you want to specify an address, use -http-addr instead)")
		flDepotPath         = flag.String("depot", envString("SCEP_FILE_DEPOT", "depot"), "path to ca folder")
		flCAPass            = flag.String("capass", envString("SCEP_CA_PASS", ""), "passwd for the ca.key")
		flClDuration        = flag.String("crtvalid", envString("SCEP_CERT_VALID", "365"), "validity for new client certificates in days")
		flClAllowRenewal    = flag.String("allowrenew", envString("SCEP_CERT_RENEW", "14"), "do not allow renewal until n days before expiry, set to 0 to always allow")
		flChallengePassword = flag.String("challenge", envString("SCEP_CHALLENGE_PASSWORD", ""), "enforce a challenge password")
		flCSRVerifierExec   = flag.String("csrverifierexec", envString("SCEP_CSR_VERIFIER_EXEC", ""), "will be passed the CSRs for verification")
		flDebug             = flag.Bool("debug", envBool("SCEP_LOG_DEBUG"), "enable debug logging")
		flLogJSON           = flag.Bool("log-json", envBool("SCEP_LOG_JSON"), "output JSON logs")
		flInitCA            = flag.Bool("init-ca", envBool("SCEP_INIT_CA"), "initialize CA if has no keys")
		flSignServerAttrs   = flag.Bool("sign-server-attrs", envBool("SCEP_SIGN_SERVER_ATTRS"), "sign cert attrs for server usage")
		flLambda            = flag.Bool("lambda", envBool("SCEP_LAMBDA"), "Run using a lambda")
		flDeleteCA          = flag.Bool("delete-ca", envBool("SCEP_DELETE_CA"), "Debug flag. Clear the CA at the beginning.")
	)
	flag.Usage = func() {
		flag.PrintDefaults()

		fmt.Println("usage: scep [<command>] [<args>]")
		fmt.Println(" ca <args> create/manage a CA")
		fmt.Println("type <command> --help to see usage for each subcommand")
	}
	flag.Parse()

	// print version information
	if *flVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	// -http-addr and -port conflict. Don't allow the user to set both.
	httpAddrSet := setByUser("http-addr", "SCEP_HTTP_ADDR")
	portSet := setByUser("port", "SCEP_HTTP_LISTEN_PORT")
	var httpAddr string
	if httpAddrSet && portSet {
		fmt.Fprintln(os.Stderr, "cannot set both -http-addr and -port")
		os.Exit(1)
	} else if httpAddrSet {
		httpAddr = *flHTTPAddr
	} else {
		httpAddr = ":" + *flPort
	}

	var logger log.Logger
	{

		if *flLogJSON {
			logger = log.NewJSONLogger(os.Stderr)
		} else {
			logger = log.NewLogfmtLogger(os.Stderr)
		}
		if !*flDebug {
			logger = level.NewFilter(logger, level.AllowInfo())
		}
		logger = log.With(logger, "ts", log.DefaultTimestampUTC)
		logger = log.With(logger, "caller", log.DefaultCaller)
	}
	lginfo := level.Info(logger)

	var err error

	// ==========
	// DEBUG
	flag.VisitAll(func(f *flag.Flag) {
		lginfo.Log("flag", f.Name, "value", f.Value)
	})

	files, err := ioutil.ReadDir(*flDepotPath)
	if err != nil {
		lginfo.Log("err", err, "msg", "trying to list files")
	}

	for _, file := range files {
		lginfo.Log("file", file.Name(), "isDir", file.IsDir())
	}

	// use this if somehow index.txt was created without the keys
	if *flDeleteCA {
		e := os.RemoveAll(*flDepotPath)
		if e != nil {
			lginfo.Log("err", e, "msg", "attempted to remove index.txt")
		}
	}

	// ===========

	var depot scepdepot.Depot // cert storage
	{
		depot, err = file.NewFileDepot(*flDepotPath)

		// if err but the initCA flag is set, create the depot
		if err != nil && *flInitCA {
			lginfo.Log("msg", "Initializing new CA")
			var caCMD = flag.NewFlagSet("ca", flag.ExitOnError)
			caMain(caCMD, os.Args[1:])
			depot, err = file.NewFileDepot(*flDepotPath)
		}

		if err != nil {
			lginfo.Log("err", err)
			os.Exit(1)
		}
	}
	allowRenewal, err := strconv.Atoi(*flClAllowRenewal)
	if err != nil {
		lginfo.Log("err", err, "msg", "No valid number for allowed renewal time")
		os.Exit(1)
	}
	clientValidity, err := strconv.Atoi(*flClDuration)
	if err != nil {
		lginfo.Log("err", err, "msg", "No valid number for client cert validity")
		os.Exit(1)
	}
	var csrVerifier csrverifier.CSRVerifier
	if *flCSRVerifierExec > "" {
		executableCSRVerifier, err := executablecsrverifier.New(*flCSRVerifierExec, lginfo)
		if err != nil {
			lginfo.Log("err", err, "msg", "Could not instantiate CSR verifier")
			os.Exit(1)
		}
		csrVerifier = executableCSRVerifier
	}

	var svc scepserver.Service // scep service
	{
		crts, key, err := depot.CA([]byte(*flCAPass))
		if err != nil {
			lginfo.Log("err", err)
			os.Exit(1)
		}
		if len(crts) < 1 {
			lginfo.Log("err", "missing CA certificate")
			os.Exit(1)
		}
		signerOpts := []scepdepot.Option{
			scepdepot.WithAllowRenewalDays(allowRenewal),
			scepdepot.WithValidityDays(clientValidity),
			scepdepot.WithCAPass(*flCAPass),
		}
		if *flSignServerAttrs {
			signerOpts = append(signerOpts, scepdepot.WithSeverAttrs())
		}
		var signer scepserver.CSRSigner = scepdepot.NewSigner(depot, signerOpts...)
		if *flChallengePassword != "" {
			signer = scepserver.ChallengeMiddleware(*flChallengePassword, signer)
		}
		if csrVerifier != nil {
			signer = csrverifier.Middleware(csrVerifier, signer)
		}
		svc, err = scepserver.NewService(crts[0], key, signer, scepserver.WithLogger(logger))
		if err != nil {
			lginfo.Log("err", err)
			os.Exit(1)
		}
		svc = scepserver.NewLoggingService(log.With(lginfo, "component", "scep_service"), svc)
	}

	var h http.Handler // http handler
	{
		e := scepserver.MakeServerEndpoints(svc)
		e.GetEndpoint = scepserver.EndpointLoggingMiddleware(lginfo)(e.GetEndpoint)
		e.PostEndpoint = scepserver.EndpointLoggingMiddleware(lginfo)(e.PostEndpoint)
		h = scepserver.MakeHTTPHandler(e, svc, log.With(lginfo, "component", "http"))
	}

	// start http server
	errs := make(chan error, 2)
	go func() {
		if *flLambda {

			// Proxies requests from the AWS API Gateway to go's http handlers
			// https://github.com/awslabs/aws-lambda-go-api-proxy
			//
			// Note that the Gateway needs to have binary media support added through the `binaryMediaTypes` parameter.
			// See this doc: https://docs.aws.amazon.com/apigateway/latest/developerguide/set-up-lambda-proxy-integrations.html
			lginfo.Log("transport", "http", "context", "lambda", "msg", "listening")
			lambda.Start(httpadapter.New(h).ProxyWithContext)
		} else {
			lginfo.Log("transport", "http", "address", httpAddr, "msg", "listening")
			errs <- http.ListenAndServe(httpAddr, h)
		}
	}()
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT)
		errs <- fmt.Errorf("%s", <-c)
	}()

	lginfo.Log("terminated", <-errs)
}

func caMain(cmd *flag.FlagSet, args []string) int {
	var (
		flDepotPath  = cmd.String("depot", envString("SCEP_FILE_DEPOT", "depot"), "path to ca folder")
		flInit       = cmd.Bool("init-ca", envBool("SCEP_INIT_CA"), "create a new CA")
		flYears      = cmd.Int("years", envInt("SCEP_CA_YEARS", 10), "default CA years")
		flKeySize    = cmd.Int("keySize", envInt("SCEP_CA_KEYSIZE", 4096), "rsa key size")
		flCommonName = cmd.String("common_name", envString("SCEP_CA_COMMON_NAME", "MICROMDM SCEP CA"), "common name (CN) for CA cert")
		flOrg        = cmd.String("organization", envString("SCEP_CA_ORG", "scep-ca"), "organization for CA cert")
		flOrgUnit    = cmd.String("organizational_unit", envString("SCEP_CA_OU", "SCEP CA"), "organizational unit (OU) for CA cert")
		flPassword   = cmd.String("key-password", envString("SCEP_CA_PASSWORD", ""), "password to store rsa key")
		flCountry    = cmd.String("country", envString("SCEP_CA_COUNTRY", "US"), "country for CA cert")
		flInlineCert = cmd.String("inline-cert", envString("SCEP_INLINE_CERT", ""), "inline CA certficiate")
		flInlineKey  = cmd.String("inline-key", envString("SCEP_INLINE_KEY", ""), "inline CA private key")
		flPathCert   = cmd.String("path-cert", envString("SCEP_PATH_CERT", ""), "inline CA certficiate")
		flPathKey    = cmd.String("path-key", envString("SCEP_PATH_KEY", ""), "inline CA private key")
	)
	cmd.Parse(args)

	if *flInit {
		fmt.Println("Initializing new CA via main CLI")
	} else {
		fmt.Println("Initializing new CA")
	}

	if *flInlineCert != "" && *flInlineKey != "" {
		fmt.Println("Storing ENV-provided cert and key to the depot")

		if err := storeFileInDepot(*flDepotPath, "ca.pem", []byte(*flInlineCert)); err != nil {
			fmt.Println(err)
			return 1
		}
		if err := storeFileInDepot(*flDepotPath, "ca.key", []byte(*flInlineKey)); err != nil {
			fmt.Println(err)
			return 1
		}
	} else if *flPathCert != "" && *flPathKey != "" {
		fmt.Println("Coping key and cert into CA depot")
		if err := copyFileToDepot(*flPathCert, *flDepotPath, "ca.pem"); err != nil {
			fmt.Println(err)
			return 1
		}
		if err := copyFileToDepot(*flPathKey, *flDepotPath, "ca.key"); err != nil {
			fmt.Println(err)
			return 1
		}
	} else {
		fmt.Println("Generating new key and cert")
		key, err := createKey(*flKeySize, []byte(*flPassword), *flDepotPath)
		if err != nil {
			fmt.Println(err)
			return 1
		}
		if err := createCertificateAuthority(key, *flYears, *flCommonName, *flOrg, *flOrgUnit, *flCountry, *flDepotPath); err != nil {
			fmt.Println(err)
			return 1
		}
	}

	return 0
}

func copyFileToDepot(sourceFile string, depotPath string, filename string) error {
	// create depot folder if missing
	if err := os.MkdirAll(depotPath, 0755); err != nil {
		return err
	}

	input, err := ioutil.ReadFile(sourceFile)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath.Join(depotPath, filename), input, 0644)
	if err != nil {
		return err
	}
	return nil
}

// create a key, save it to depot and return it for further usage.
func createKey(bits int, password []byte, depot string) (*rsa.PrivateKey, error) {
	// create depot folder if missing
	if err := os.MkdirAll(depot, 0755); err != nil {
		return nil, err
	}
	name := filepath.Join(depot, "ca.key")
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// create RSA key and save as PEM file
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, err
	}
	privPEMBlock, err := x509.EncryptPEMBlock(
		rand.Reader,
		rsaPrivateKeyPEMBlockType,
		x509.MarshalPKCS1PrivateKey(key),
		password,
		x509.PEMCipher3DES,
	)
	if err != nil {
		return nil, err
	}
	if err := pem.Encode(file, privPEMBlock); err != nil {
		os.Remove(name)
		return nil, err
	}

	return key, nil
}

func storeFileInDepot(depot string, filename string, data []byte) error {
	// create depot folder if missing
	if err := os.MkdirAll(depot, 0755); err != nil {
		return err
	}
	name := filepath.Join(depot, filename)
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(name)
		return err
	}

	return nil
}

func createCertificateAuthority(key *rsa.PrivateKey, years int, commonName string, organization string, organizationalUnit string, country string, depot string) error {
	cert := scepdepot.NewCACert(
		scepdepot.WithYears(years),
		scepdepot.WithCommonName(commonName),
		scepdepot.WithOrganization(organization),
		scepdepot.WithOrganizationalUnit(organizationalUnit),
		scepdepot.WithCountry(country),
	)
	crtBytes, err := cert.SelfSign(rand.Reader, &key.PublicKey, key)
	if err != nil {
		return err
	}

	return storeFileInDepot(depot, "ca.pem", pemCert(crtBytes))
}

const (
	rsaPrivateKeyPEMBlockType = "RSA PRIVATE KEY"
	certificatePEMBlockType   = "CERTIFICATE"
)

func pemCert(derBytes []byte) []byte {
	pemBlock := &pem.Block{
		Type:    certificatePEMBlockType,
		Headers: nil,
		Bytes:   derBytes,
	}
	out := pem.EncodeToMemory(pemBlock)
	return out
}

func envString(key string, def string) string {
	if env := os.Getenv(key); env != "" {
		return env
	}
	return def
}

func envInt(key string, def int) int {
	if env := os.Getenv(key); env != "" {
		convertedEnv, err := strconv.Atoi(env)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		return convertedEnv
	}
	return def
}

func envBool(key string) bool {
	if env := os.Getenv(key); env == "true" {
		return true
	}
	return false
}

func setByUser(flagName, envName string) bool {
	userDefinedFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		userDefinedFlags[f.Name] = true
	})
	flagSet := userDefinedFlags[flagName]
	_, envSet := os.LookupEnv(envName)
	return flagSet || envSet
}
