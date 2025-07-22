package services

import (
	"bulk_issuance/config"
	"bulk_issuance/utils"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-openapi/spec"
)

func getSchemaPropertyNames(schemaName string) ([]string, error) {
	schemaProperties, err := getSchemaProperties(schemaName)
	if err == nil {
		properties := make([]string, 0)
		for k := range schemaProperties {
			properties = append(properties, k)
		}
		sort.Strings(properties)
		return properties, nil
	} else {
		return nil, err
	}
}

func getSchemaProperties(schemaName string) (spec.SchemaProperties, error) {
	registrySwaggerSpecification := getSwaggerJson()
	schemaDefinition, ok := registrySwaggerSpecification.Definitions[schemaName]
	if !ok {
		return nil, errors.New(schemaName + "schema not found")
	}
	return schemaDefinition.Properties, nil
}

func getSwaggerJson() spec.Swagger {
	resp, err := http.Get(config.Config.Registry.BaseUrl + "api/docs/swagger.json")
	utils.LogErrorIfAny("Error creating a get request for %v : %v", err, config.Config.Registry.BaseUrl+"api/docs/swagger.json")
	body, _ := io.ReadAll(resp.Body)
	var responseMap spec.Swagger
	err = json.Unmarshal(body, &responseMap)
	utils.LogErrorIfAny("Error creating request body for %v : %v", err, config.Config.Registry.BaseUrl+"api/docs/swagger.json")
	return responseMap
}

// flattenSchemaProperties recursively flattens schema properties for CSV headers and sample values.
func flattenSchemaProperties(
	prefix string,
	properties spec.SchemaProperties,
	resultHeaders *[]string,
	resultSamples *[]string,
) {
	for key, value := range properties {
		// Compose the full property name
		var fullKey string
		if prefix != "" {
			fullKey = prefix + "." + key
		} else {
			fullKey = key
		}

		// If the property is an object, recurse
		if len(value.Type) > 0 && value.Type[0] == "object" && value.Properties != nil {
			flattenSchemaProperties(fullKey, value.Properties, resultHeaders, resultSamples)
		} else {
			*resultHeaders = append(*resultHeaders, fullKey)
			*resultSamples = append(*resultSamples, utils.GetSampleValueByType(value))
		}
	}
}

func getSchemaPropertiesAndSampleValues(schemaName string) ([]string, []string, error) {
	schemaProperties, err := getSchemaProperties(schemaName)
	if err != nil {
		return nil, nil, err
	}
	headers := make([]string, 0)
	samples := make([]string, 0)
	flattenSchemaProperties("", schemaProperties, &headers, &samples)
	return headers, samples, nil
}

// Fetches the full schema for a given schemaName from the registry service using an authorization token.
func getFullSchemaFromRegistry(schemaName string, token string) (map[string]interface{}, error) {
	url := config.Config.Registry.BaseUrl + "registry/" + schemaName
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		if !strings.HasPrefix(token, "Bearer ") {
			token = "Bearer " + token
		}
		req.Header.Set("Authorization", token)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to fetch schema: %s, status: %d", schemaName, resp.StatusCode)
	}
	var schema map[string]interface{}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&schema); err != nil {
		return nil, err
	}
	return schema, nil
}
