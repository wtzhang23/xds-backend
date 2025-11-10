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

	"gopkg.in/yaml.v2"
)

//go:embed templates/*
var templateFS embed.FS

// TemplateData holds the data for template rendering
type TemplateData struct {
	XdsBackendGroup                string
	XdsBackendAPIVersion           string
	XdsBackendKind                 string
	XdsBackendResourceName         string
	EnvoyGatewayNamespace          string
	TestServiceName                string
	TestNamespace                  string
	GatewayClassName               string
	GatewayName                    string
	HTTPRouteName                  string
	GatewayListenerName            string
	GatewayListenerPort            int
	ExtensionServerFQDN            string
	ExtensionServerPort            int
	ExtensionServerImageRepo       string
	ExtensionServerImageTag        string
	ImagePullPolicy                string
	EnvoyGatewayContainerPort      int
	EnvoyGatewayHTTPSContainerPort int
	TestServicePort                int
	TestServiceIP                  string // IP address of the test service
	EdsConfigPath                  string // Path to EDS config file in Envoy pod
	EdsConfigMapName               string // Name of ConfigMap containing EDS config
}

// indent adds indentation to each line of a string
func indent(s string, spaces int) string {
	if s == "" {
		return ""
	}
	indentStr := strings.Repeat(" ", spaces)
	lines := strings.Split(s, "\n")
	var result strings.Builder
	for i, line := range lines {
		if line != "" {
			result.WriteString(indentStr)
			result.WriteString(line)
		}
		if i < len(lines)-1 {
			result.WriteString("\n")
		}
	}
	return result.String()
}

// LoadTemplate loads and renders a template file with the given data
// templatePath should be just the filename (e.g., "gateway.yaml") or "templates/gateway.yaml"
func LoadTemplate(templatePath string, data TemplateData) (string, error) {
	// Normalize the path to use "templates/" prefix
	templateName := filepath.Base(templatePath)
	embeddedPath := filepath.Join("templates", templateName)

	// Read the template file from embedded FS
	templateBytes, err := templateFS.ReadFile(embeddedPath)
	if err != nil {
		return "", fmt.Errorf("failed to read embedded template file %s: %w", embeddedPath, err)
	}

	// Parse the template with custom functions
	funcMap := template.FuncMap{
		"indent": indent,
	}
	tmpl, err := template.New(templateName).Funcs(funcMap).Parse(string(templateBytes))
	if err != nil {
		return "", fmt.Errorf("failed to parse template %s: %w", embeddedPath, err)
	}

	// Render the template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", embeddedPath, err)
	}

	result := buf.String()

	// Write rendered config to file for debugging
	if err := writeRenderedConfig(templatePath, result); err != nil {
		// Log but don't fail on write errors
		fmt.Printf("[TemplateLoader] Warning: Failed to write rendered config: %v\n", err)
	}

	return result, nil
}

// convertToMapStringInterface recursively converts map[interface{}]interface{} to map[string]interface{}
// This is needed because yaml.v2 unmarshals to map[interface{}]interface{}, but Helm needs map[string]interface{}
func convertToMapStringInterface(in interface{}) interface{} {
	switch v := in.(type) {
	case map[interface{}]interface{}:
		result := make(map[string]interface{})
		for k, val := range v {
			result[fmt.Sprintf("%v", k)] = convertToMapStringInterface(val)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, val := range v {
			result[i] = convertToMapStringInterface(val)
		}
		return result
	default:
		return in
	}
}

// LoadHelmValues loads Helm values from a YAML template and returns them as a map
func LoadHelmValues(templatePath string, data TemplateData) (map[string]interface{}, error) {
	// Load and render the template
	yamlContent, err := LoadTemplate(templatePath, data)
	if err != nil {
		return nil, fmt.Errorf("failed to load template: %w", err)
	}

	// Parse YAML into map (yaml.v2 produces map[interface{}]interface{})
	var rawValues interface{}
	if err := yaml.Unmarshal([]byte(yamlContent), &rawValues); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	// Convert to map[string]interface{} for Helm compatibility
	converted := convertToMapStringInterface(rawValues)
	values, ok := converted.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("expected map[string]interface{}, got %T", converted)
	}

	// Write rendered Helm values to file for debugging
	if err := writeRenderedHelmValues(templatePath, values); err != nil {
		// Log but don't fail on write errors
		fmt.Printf("[TemplateLoader] Warning: Failed to write rendered Helm values: %v\n", err)
	}

	return values, nil
}

// writeRenderedConfig writes a rendered template to a file in .rendered-configs/
func writeRenderedConfig(templatePath, content string) error {
	// Get the directory of the current file (template_loader.go)
	_, callerFile, _, _ := runtime.Caller(0)
	baseDir := filepath.Dir(callerFile)
	renderedDir := filepath.Join(baseDir, ".rendered-configs")
	if err := os.MkdirAll(renderedDir, 0755); err != nil {
		return fmt.Errorf("failed to create rendered configs directory: %w", err)
	}

	// Get the template filename without path
	templateName := filepath.Base(templatePath)
	// Add timestamp to avoid conflicts
	timestamp := time.Now().Format("20060102-150405")
	outputFilename := fmt.Sprintf("%s-%s", timestamp, templateName)
	outputPath := filepath.Join(renderedDir, outputFilename)

	return os.WriteFile(outputPath, []byte(content), 0644)
}

// writeRenderedHelmValues writes rendered Helm values to a file in .rendered-configs/
func writeRenderedHelmValues(templatePath string, values map[string]interface{}) error {
	// Get the directory of the current file (template_loader.go)
	_, callerFile, _, _ := runtime.Caller(0)
	baseDir := filepath.Dir(callerFile)
	renderedDir := filepath.Join(baseDir, ".rendered-configs")
	if err := os.MkdirAll(renderedDir, 0755); err != nil {
		return fmt.Errorf("failed to create rendered configs directory: %w", err)
	}

	// Get the template filename without path
	templateName := filepath.Base(templatePath)
	// Add timestamp to avoid conflicts
	timestamp := time.Now().Format("20060102-150405")
	outputFilename := fmt.Sprintf("%s-%s.yaml", timestamp, templateName)
	outputPath := filepath.Join(renderedDir, outputFilename)

	// Convert to YAML
	yamlBytes, err := yaml.Marshal(values)
	if err != nil {
		return fmt.Errorf("failed to marshal Helm values to YAML: %w", err)
	}

	return os.WriteFile(outputPath, yamlBytes, 0644)
}

// GetTemplatePath returns the template filename for use with LoadTemplate
// Since templates are now embedded, this just returns the filename
func GetTemplatePath(filename string) string {
	// Just return the filename - templates are embedded and accessed via "templates/" prefix
	return filename
}
