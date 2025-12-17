package e2e

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/onsi/ginkgo/v2"
	"gopkg.in/yaml.v2"
	k8syaml "sigs.k8s.io/yaml"
)

//go:embed templates/*
var templateFS embed.FS

type GatewayClassTemplate struct {
	GatewayClassName      string
	EnvoyGatewayNamespace string
}

type GatewayTemplate struct {
	GatewayName           string
	EnvoyGatewayNamespace string
	GatewayClassName      string
	GatewayListenerName   string
	GatewayListenerPort   int
}

type BackendRef struct {
	Group     string
	Kind      string
	Name      string
	Namespace string
	Weight    int
}

type HTTPRouteTemplate struct {
	HTTPRouteName         string
	GatewayName           string
	EnvoyGatewayNamespace string
	HTTPRoutePathPrefix   string
	// For single backendRef
	XdsBackendGroup        string
	XdsBackendKind         string
	XdsBackendResourceName string
	// For multiple backendRefs
	BackendRefs []BackendRef
}

type XdsBackendTemplate struct {
	XdsBackendGroup        string
	XdsBackendAPIVersion   string
	XdsBackendKind         string
	XdsBackendResourceName string
	EnvoyGatewayNamespace  string
	EdsConfigPath          string
	TestServiceName        string
}

type XdsBackendFileXdsTemplate struct {
	XdsBackendGroup        string
	XdsBackendAPIVersion   string
	XdsBackendKind         string
	XdsBackendResourceName string
	EnvoyGatewayNamespace  string
	FileXdsClusterName     string
	TestServiceName        string
	// TLS configuration (optional)
	TlsEnabled    bool   // Set to true to enable TLS section. If true but no TLS fields provided, outputs empty TLS config (tls: {})
	TlsCaCertName string // Optional: CA certificate name for TLS
	TlsCaCertEmpty bool  // Set to true to output empty caCertificates block (caCertificates: {})
	TlsHostname   string // Optional: Hostname for TLS
}

// XdsBackendInlineTLSTemplate is deprecated, use XdsBackendFileXdsTemplate with TlsEnabled=true
type XdsBackendInlineTLSTemplate struct {
	XdsBackendGroup        string
	XdsBackendAPIVersion   string
	XdsBackendKind         string
	XdsBackendResourceName string
	EnvoyGatewayNamespace  string
	FileXdsClusterName     string
	TestServiceName        string
	TlsCaCertName          string
	TlsHostname            string
}

// XdsBackendInsecureTLSTemplate is deprecated, use XdsBackendFileXdsTemplate with TlsEnabled=true (and no TlsCaCertName/TlsHostname for empty config)
type XdsBackendInsecureTLSTemplate struct {
	XdsBackendGroup        string
	XdsBackendAPIVersion   string
	XdsBackendKind         string
	XdsBackendResourceName string
	EnvoyGatewayNamespace  string
	FileXdsClusterName     string
	TestServiceName        string
}

type FileXdsConfigMapTemplate struct {
	FileXdsConfigMapName  string
	EnvoyGatewayNamespace string
	TestServiceName       string
	TestServiceIP         string
	TestServicePort       int
	TestServiceTLSPort    int
	TlsCaCertPEM          string
	TlsCaCertName         string
}

type FileXdsServerConfigMapTemplate struct {
	FileXdsServerConfigMapName string
	EnvoyGatewayNamespace      string
	Services                   []ServiceEndpoint
	TlsCaCertPEM               string
	TlsCaCertName              string
}

type ServiceEndpoint struct {
	ServiceName string
	ServiceIP   string
	ServicePort int
}

type EnvoyEdsConfigMapTemplate struct {
	EnvoyEdsConfigMapName string
	EnvoyGatewayNamespace string
	TestServiceName       string
	TestServiceIP         string
	TestServicePort       int
}

type EnvoyProxyTemplate struct {
	GatewayClassName      string
	EnvoyGatewayNamespace string
	EnvoyEdsConfigMapName string
	FileXdsClusterName    string
	FileXdsServiceFQDN    string
	FileXdsPort           int
}

type FileXdsDeploymentTemplate struct {
	FileXdsDeploymentName      string
	EnvoyGatewayNamespace      string
	ExtensionServerImageRepo   string
	ExtensionServerImageTag    string
	ImagePullPolicy            string
	FileXdsPort                int
	FileXdsConfigPath          string
	FileXdsConfigDir           string
	FileXdsServerConfigMapName string
}

type FileXdsServiceTemplate struct {
	FileXdsServiceName    string
	FileXdsDeploymentName string
	EnvoyGatewayNamespace string
	FileXdsPort           int
}

type TestServiceTLSSecretTemplate struct {
	TestServiceTLSSecretName string
	TestNamespace            string
	TestServiceTLSCertBase64 string
	TestServiceTLSKeyBase64  string
}

type BackendTLSCACertConfigMapTemplate struct {
	BackendTLSCACertName  string
	EnvoyGatewayNamespace string
	BackendTLSCACertPEM   string
}

type BackendTLSPolicyTemplate struct {
	BackendTLSPolicyName   string
	EnvoyGatewayNamespace  string
	XdsBackendGroup        string
	XdsBackendKind         string
	XdsBackendResourceName string
	BackendTLSHostname     string
	BackendTLSCACertName   string
}

type TestServiceDeploymentTemplate struct {
	TestServiceName          string
	TestNamespace            string
	TestServicePort          int
	TestServiceTLSSecretName string
	TestServiceTLSPort       int
	TestServiceResponse      string
}

type ExtensionServerValuesTemplate struct {
	ExtensionServerImageRepo       string
	ExtensionServerImageTag        string
	ImagePullPolicy                string
	ExtensionServerEnablePlaintext bool
	ExtensionServerTLSEnabled      bool
	ExtensionServerTLSPort         int
	ExtensionServerTLSSecretName   string
}

type EnvoyGatewayValuesTemplate struct {
	XdsBackendGroup                string
	XdsBackendAPIVersion           string
	ExtensionServerFQDN            string
	ExtensionServerPort            int
	EnvoyGatewayContainerPort      int
	EnvoyGatewayHTTPSContainerPort int
	EnvoyGatewayImageRepository    string
	EnvoyGatewayImageTag           string
}

type KindClusterConfigTemplate struct {
	ClusterName                    string
	EnvoyGatewayContainerPort      int
	EnvoyGatewayHostPort           int
	EnvoyGatewayHTTPSContainerPort int
	EnvoyGatewayHTTPSHostPort      int
}

func LoadTemplate(templatePath string, data interface{}) (string, error) {
	templateName := filepath.Base(templatePath)
	embeddedPath := filepath.Join("templates", templateName)

	templateBytes, err := templateFS.ReadFile(embeddedPath)
	if err != nil {
		return "", fmt.Errorf("failed to read embedded template file %s: %w", embeddedPath, err)
	}

	funcMap := template.FuncMap{
		"yamlEscape": func(text string) string {
			// Escape special YAML characters for quoted string
			text = strings.ReplaceAll(text, "\\", "\\\\")
			text = strings.ReplaceAll(text, "\"", "\\\"")
			text = strings.ReplaceAll(text, "\n", "\\n")
			return text
		},
	}
	tmpl, err := template.New(templateName).Funcs(funcMap).Parse(string(templateBytes))
	if err != nil {
		return "", fmt.Errorf("failed to parse template %s: %w", embeddedPath, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", embeddedPath, err)
	}

	result := buf.String()

	if err := writeRenderedConfig(templatePath, result); err != nil {
		fmt.Printf("[TemplateLoader] Warning: Failed to write rendered config: %v\n", err)
	}

	return result, nil
}

func LoadHelmValues(templatePath string, data interface{}) (map[string]interface{}, error) {
	yamlContent, err := LoadTemplate(templatePath, data)
	if err != nil {
		return nil, fmt.Errorf("failed to load template: %w", err)
	}

	var values map[string]interface{}
	if err := k8syaml.Unmarshal([]byte(yamlContent), &values); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if err := writeRenderedHelmValues(templatePath, values); err != nil {
		fmt.Printf("[TemplateLoader] Warning: Failed to write rendered Helm values: %v\n", err)
	}

	return values, nil
}

func getRenderedConfigPath(templatePath string, extension string, baseDir string) (string, error) {
	testName := "setup"
	spec := ginkgo.CurrentSpecReport()
	if spec.FullText() != "" {
		testName = sanitizeTestName(spec.FullText())
	}

	renderedDir := filepath.Join(baseDir, ".rendered-configs", testName)
	if err := os.MkdirAll(renderedDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create rendered configs directory: %w", err)
	}

	templateName := filepath.Base(templatePath)
	timestamp := time.Now().Format(LogTimestampFormat)
	outputFilename := fmt.Sprintf("%s-%s%s", timestamp, templateName, extension)
	return filepath.Join(renderedDir, outputFilename), nil
}

func writeRenderedConfig(templatePath, content string) error {
	_, callerFile, _, _ := runtime.Caller(0)
	baseDir := filepath.Dir(callerFile)

	outputPath, err := getRenderedConfigPath(templatePath, "", baseDir)
	if err != nil {
		return err
	}
	return os.WriteFile(outputPath, []byte(content), 0644)
}

// writeRenderedHelmValues writes rendered Helm values to a file in .rendered-configs/
func writeRenderedHelmValues(templatePath string, values map[string]interface{}) error {
	// Get the directory of the current file (template_loader.go)
	_, callerFile, _, _ := runtime.Caller(0)
	baseDir := filepath.Dir(callerFile)

	// Don't add .yaml extension if template path already has it
	extension := ""
	if !strings.HasSuffix(templatePath, ".yaml") && !strings.HasSuffix(templatePath, ".yml") {
		extension = ".yaml"
	}
	outputPath, err := getRenderedConfigPath(templatePath, extension, baseDir)
	if err != nil {
		return err
	}

	// Convert to YAML
	yamlBytes, err := yaml.Marshal(values)
	if err != nil {
		return fmt.Errorf("failed to marshal Helm values to YAML: %w", err)
	}

	return os.WriteFile(outputPath, yamlBytes, 0644)
}

func sanitizeTestName(testName string) string {
	sanitized := strings.ReplaceAll(testName, " ", "-")
	sanitized = strings.ReplaceAll(sanitized, "/", "-")
	sanitized = strings.ReplaceAll(sanitized, "\\", "-")
	sanitized = strings.ReplaceAll(sanitized, ":", "-")
	sanitized = strings.ReplaceAll(sanitized, "*", "-")
	sanitized = strings.ReplaceAll(sanitized, "?", "-")
	sanitized = strings.ReplaceAll(sanitized, "\"", "-")
	sanitized = strings.ReplaceAll(sanitized, "<", "-")
	sanitized = strings.ReplaceAll(sanitized, ">", "-")
	sanitized = strings.ReplaceAll(sanitized, "|", "-")
	for strings.Contains(sanitized, "--") {
		sanitized = strings.ReplaceAll(sanitized, "--", "-")
	}
	sanitized = strings.Trim(sanitized, "-")
	if len(sanitized) > 200 {
		sanitized = sanitized[:200]
	}
	return sanitized
}
