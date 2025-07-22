package services

import (
	"bulk_issuance/config"
	"bulk_issuance/db"
	"bulk_issuance/swagger_gen/models"
	"bulk_issuance/utils"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

var client = &http.Client{}

type Services struct {
	repo db.IRepo
}

type IService interface {
	GetSampleCSVForSchema(schemaName string) (*bytes.Buffer, error)
	InsertIntoFileData(rows [][]string, fileName string, header string, principal *models.JWTClaimBody) (uint, error)
	ProcessDataFromCSV(header http.Header, vcName string, file io.Reader) (int, int, int, [][]string, string, error)
	GetCSVReport(id int, userId string) (*string, *bytes.Buffer, error)
	GetUploadedFiles(userId string, limit *int64, offset *int64) ([]*models.UploadedFileDTO, error)
}

func Init(repository db.IRepo) IService {
	services := Services{
		repo: repository,
	}
	return &services
}

func (services *Services) InsertIntoFileData(rows [][]string, fileName string, header string, principal *models.JWTClaimBody) (uint, error) {
	log.Info("adding entry to dbFileData")
	rowBytes, err := json.Marshal(rows)
	utils.LogErrorIfAny("Error while marshalling data for database : %v", err)
	fileUpload := db.UploadedFile{
		Filename:     fileName,
		Headers:      header,
		TotalRecords: len(rows),
		RowData:      rowBytes,
		UserID:       principal.UserID,
		UserName:     principal.PreferredUsername,
		Date:         time.Now().Format("2006-01-02"),
	}

	return services.repo.Insert(&fileUpload)
}

func (services *Services) ProcessDataFromCSV(header http.Header, schemaName string, file io.Reader) (int, int, int, [][]string, string, error) {
	csvScanner, err := NewScanner(file)
	if err != nil {
		return 0, 0, 0, nil, "", err
	}
	var (
		totalCreated = 0
		totalUpdated = 0
		totalErrors  = 0
	)
	rows := make([][]string, 0)
	log.Info("processing all rows from csv with intelligent create/update logic")

	authorizationToken := header["Authorization"][0]

	// Fetch uniqueIndexFields from registry schema
	uniqueIndexFields, err := getUniqueIndexFieldsFromRegistry(schemaName, authorizationToken)
	if err != nil {
		return 0, 0, 0, nil, "", fmt.Errorf("failed to fetch unique index fields: %v", err)
	}

	for csvScanner.Scan() {
		currRow := csvScanner.Row

		// Extract unique identifier for the entity (now dynamic)
		identifier, err := extractUniqueIdentifierDynamic(currRow, csvScanner.Head, uniqueIndexFields)
		if err != nil {
			// If we can't extract identifier, treat as error
			csvScanner.appendHeader("Errors")
			currRow = append(currRow, "Identifier extraction failed: "+err.Error())
			totalErrors += 1
			rows = append(rows, currRow)
			continue
		}

		// Search for existing record
		searchBody := buildSearchBodyDynamic(uniqueIndexFields, identifier)
		searchResp, searchErr := callRegistrySearchAPI(schemaName, searchBody, authorizationToken)

		var res *http.Response
		var operationType string

		if searchErr != nil {
			// Search failed, treat as new record and create
			log.Warnf("Search failed for %s with identifier %s, creating new record: %v", schemaName, identifier, searchErr)
			schemaRequest := createSchemaRequest(currRow, csvScanner.Head)
			res, err = callRegistryAPI(schemaName, schemaRequest, authorizationToken)
			operationType = "CREATE"
		} else {
			// Search successful, check if record exists
			osid := extractOsidFromSearch(schemaName, searchResp)
			log.Infof("Search result for %s with identifier %s: osid=%s", schemaName, identifier, osid)
			if osid == "" {
				// Record not found, create new
				log.Infof("No existing record found for %s with identifier %s, creating new record", schemaName, identifier)
				schemaRequest := createSchemaRequest(currRow, csvScanner.Head)
				res, err = callRegistryAPI(schemaName, schemaRequest, authorizationToken)
				operationType = "CREATE"
			} else {
				// Record found, update existing
				log.Infof("Existing record found for %s with identifier %s, updating record", schemaName, identifier)
				schemaRequest := createSchemaRequest(currRow, csvScanner.Head)
				// removeFieldsForUpdate(schemaName, schemaRequest, authorizationToken)
				res, err = callRegistryUpdateAPI(schemaName, osid, schemaRequest, authorizationToken)
				operationType = "UPDATE"
			}
		}

		if err != nil {
			utils.LogErrorIfAny("Error in "+operationType+" operation for record with identifier "+identifier+": %v", err)
		}

		if res.StatusCode != 200 {
			csvScanner.appendHeader("Errors")
			currRow = appendErrorsToCurrentRow(res, currRow)
			totalErrors += 1
		} else {
			if operationType == "CREATE" {
				totalCreated += 1
			} else {
				totalUpdated += 1
			}
		}
		rows = append(rows, currRow)
	}
	log.Info("processed all rows from csv")
	return totalCreated, totalUpdated, totalErrors, rows, csvScanner.getHeaderAsString(), nil
}

func callRegistryAPI(schemaName string, schemaRequest map[string]interface{}, token string) (*http.Response, error) {
	methodName := "POST"
	postBody, err := json.Marshal(schemaRequest)
	utils.LogErrorIfAny("Error in creating request %v : %v", err, config.Config.Registry.BaseUrl+"api/v1/"+schemaName)

	req, err := http.NewRequest(methodName, config.Config.Registry.BaseUrl+"api/v1/"+schemaName, bytes.NewBuffer(postBody))
	utils.LogErrorIfAny("Error in creating request %v : %v", err, config.Config.Registry.BaseUrl+"api/v1/"+schemaName)

	// Ensure token has Bearer prefix
	if !strings.HasPrefix(token, "Bearer ") {
		token = "Bearer " + token
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Errorf("Error calling registry API: %v", err)
		return nil, err
	}

	return resp, err
}

func createSchemaRequest(row []string, head map[string]int) map[string]interface{} {
	jsonBody := make(map[string]interface{})
	for header, index := range head {
		if index >= len(row) {
			continue
		}
		// Sanitize the value
		value := strings.TrimSpace(row[index])

		// Split header by '.' for nesting
		parts := strings.Split(header, ".")

		if len(parts) == 1 {
			// No nesting, add directly
			jsonBody[parts[0]] = value
		} else {
			// Nested field
			parentKey := parts[0]
			childKey := parts[1]

			// Create the parent map if it doesn't exist
			if _, ok := jsonBody[parentKey]; !ok {
				jsonBody[parentKey] = make(map[string]interface{})
			}

			// Add the value to the nested map
			nestedMap := jsonBody[parentKey].(map[string]interface{})
			nestedMap[childKey] = value
		}
	}
	return jsonBody
}

func appendErrorsToCurrentRow(res *http.Response, currRow []string) []string {
	resBody, err := io.ReadAll(res.Body)
	utils.LogErrorIfAny("Error while reading error response from adding single record : %v", err)

	if len(resBody) == 0 {
		currRow = append(currRow, "Empty error response from server")
		return currRow
	}

	var responseMap map[string]interface{}
	err = json.Unmarshal(resBody, &responseMap)
	if err != nil {
		log.Errorf("Unmarshal Error : %v", err)
		currRow = append(currRow, "Invalid error response from server")
		return currRow
	}

	// Safely check if responseMap is nil
	if responseMap == nil {
		currRow = append(currRow, "No error message in response")
		return currRow
	}

	params, ok := responseMap["params"].(map[string]interface{})
	if !ok || params == nil || params["errmsg"] == nil {
		currRow = append(currRow, "No error message in response")
		return currRow
	}
	currRow = append(currRow, fmt.Sprintf("%v", params["errmsg"]))
	return currRow
}

// Helper to fetch uniqueIndexFields from registry API
func getUniqueIndexFieldsFromRegistry(schemaName string, token string) ([]string, error) {
	schemaResp, err := getFullSchemaFromRegistry(schemaName, token)
	if err != nil {
		return nil, err
	}
	result, ok := schemaResp["result"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("missing result in schema response")
	}
	osSchemaConfig, ok := result["osSchemaConfiguration"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("missing osSchemaConfiguration in schema response")
	}
	uniqueIndexFieldsIface, ok := osSchemaConfig["uniqueIndexFields"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("missing uniqueIndexFields in schema response")
	}
	var uniqueIndexFields []string
	for _, f := range uniqueIndexFieldsIface {
		if s, ok := f.(string); ok {
			// Handle case like "(name_en, dateOfBirth, gender)"
			s = strings.TrimSpace(s)
			if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
				// Remove parentheses and split by comma
				inner := s[1 : len(s)-1]
				fields := strings.Split(inner, ",")
				for _, field := range fields {
					uniqueIndexFields = append(uniqueIndexFields, strings.TrimSpace(field))
				}
			} else {
				uniqueIndexFields = append(uniqueIndexFields, s)
			}
		}
	}
	if len(uniqueIndexFields) == 0 {
		return nil, fmt.Errorf("no uniqueIndexFields defined in schema")
	}
	return uniqueIndexFields, nil
}

// Extracts unique identifier from a row using dynamic uniqueIndexFields
func extractUniqueIdentifierDynamic(row []string, headers map[string]int, uniqueIndexFields []string) (string, error) {
	var identifierParts []string
	for _, field := range uniqueIndexFields {
		idx, ok := headers[field]
		if !ok || idx >= len(row) {
			return "", fmt.Errorf("unique field %s not found in CSV", field)
		}
		identifierParts = append(identifierParts, strings.TrimSpace(row[idx]))
	}
	return strings.Join(identifierParts, "|"), nil // or use a tuple/JSON as needed
}

// Build search body for dynamic unique index fields
func buildSearchBodyDynamic(uniqueIndexFields []string, identifier string) map[string]interface{} {
	filters := map[string]interface{}{}
	identifierParts := strings.Split(identifier, "|")
	for i, field := range uniqueIndexFields {
		filters[field] = map[string]interface{}{"eq": identifierParts[i]}
	}
	return map[string]interface{}{
		"offset":  0,
		"limit":   1,
		"filters": filters,
	}
}

// callRegistrySearchAPI calls the registry search endpoint
func callRegistrySearchAPI(entityName string, body map[string]interface{}, token string) (map[string]interface{}, error) {
	url := config.Config.Registry.BaseUrl + "api/v1/" + entityName + "/search"

	b, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Errorf("Error reading response body: %v", err)
		return nil, err
	}

	// Check if response body is empty
	if len(bodyBytes) == 0 {
		log.Warnf("Empty response body from search API for %s", entityName)
		return map[string]interface{}{"data": []interface{}{}}, nil
	}

	var result map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		log.Errorf("Search error decoding response: %v", err)
		return nil, err
	}
	return result, nil
}

// extractOsidFromSearch extracts the osid from the search response
func extractOsidFromSearch(entityName string, searchResp map[string]interface{}) string {
	data, ok := searchResp["data"].([]interface{})
	if !ok || len(data) == 0 {
		return ""
	}
	entity, ok := data[0].(map[string]interface{})
	if !ok {
		return ""
	}
	osid, _ := entity["osid"].(string)
	return osid
}

// removeFieldsForUpdate removes unique index fields from the update body as per entity type
// func removeFieldsForUpdate(entityName string, body map[string]interface{}, token string) {
// 	uniqueIndexFields, err := getUniqueIndexFieldsFromRegistry(entityName, token)
// 	if err != nil {
// 		// fallback: do nothing if uniqueIndexFields can't be fetched
// 		return
// 	}
// 	for _, field := range uniqueIndexFields {
// 		delete(body, field)
// 	}
// }

// callRegistryUpdateAPI calls the registry update endpoint
func callRegistryUpdateAPI(entityName, osid string, body map[string]interface{}, token string) (*http.Response, error) {
	url := config.Config.Registry.BaseUrl + "api/v1/" + entityName + "/" + osid
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", url, bytes.NewBuffer(b))

	// Ensure token has Bearer prefix
	if !strings.HasPrefix(token, "Bearer ") {
		token = "Bearer " + token
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}
