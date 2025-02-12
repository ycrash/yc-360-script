package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"text/template"
	"time"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/go-resty/resty/v2"
)

const (
	BoomiMaxRecords             = 100
	BoomiURL                    = "https://api.boomi.com/api/rest/v1/{accountId}/ExecutionRecord/query"
	BoomiRequestContentType     = "Content-Type"
	BoomiRequestApplicationJSON = "application/json"
	BoomiRequestAccept          = "Accept"
	MaxIteration                = 1
)

type BoomiExecutionRecordQueryResult struct {
	Type            string            `json:"@type"`
	Result          []ExecutionRecord `json:"result"`
	QueryToken      string            `json:"queryToken"`
	NumberOfResults int               `json:"numberOfResults"`
}

type ExecutionRecord struct {
	Type                      string        `json:"@type"`
	ExecutionID               string        `json:"executionId"`
	Account                   string        `json:"account"`
	ExecutionTime             string        `json:"executionTime"`
	Status                    string        `json:"status"`
	ExecutionType             string        `json:"executionType"`
	ProcessName               string        `json:"processName"`
	ProcessID                 string        `json:"processId"`
	AtomName                  string        `json:"atomName"`
	AtomID                    string        `json:"atomId"`
	InboundDocumentCount      int           `json:"inboundDocumentCount"`
	InboundErrorDocumentCount int           `json:"inboundErrorDocumentCount"`
	OutboundDocumentCount     int           `json:"outboundDocumentCount"`
	ExecutionDuration         []interface{} `json:"executionDuration"`
	InboundDocumentSize       []interface{} `json:"inboundDocumentSize"`
	OutboundDocumentSize      []interface{} `json:"outboundDocumentSize"`
	RecordedDate              string        `json:"recordedDate"`
}

type AsyncAtomDiskspaceTokenResult struct {
	Type   string                 `json:"@type"`
	Result []AsyncTokenDiskResult `json:"result"`
}

type AsyncTokenDiskResult struct {
	Type    string `json:"@type"`
	File    string `json:"file"`
	RawSize int    `json:"rawSize"`
	Size    string `json:"size"`
}

type AsyncToken struct {
	Type  string `json:"@type"`
	Token string `json:"token"`
}

type AtomQuery struct {
	Type             string `json:"@type"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	AtomType         string `json:"type"`
	HostName         string `json:"hostName"`
	DateInstalled    string `json:"dateInstalled"`
	CurrentVersion   string `json:"currentVersion"`
	ForceRestartTime int    `json:"forceRestartTime"`
	ID               string `json:"id"`
}

type AtomQueryResult struct {
	Type   string      `json:"@type"`
	Result []AtomQuery `json:"result"`
}

type AtomConnectorRecordQueryResult struct {
	Type            string          `json:"@type"`
	Result          []AtomConnector `json:"result"`
	QueryToken      string          `json:"queryToken"`
	NumberOfResults int             `json:"numberOfResults"`
}

type AtomConnector struct {
	Type     string `json:"@type"`
	AtomType string `json:"type"`
	Name     string `json:"name"`
}

func CaptureBoomiDetails(endpoint string, timestamp string, pid int) {
	// get Boomi details from the config
	boomiURL := BoomiURL //config.GlobalConfig.BoomiUrl
	if boomiURL == "" {
		logger.Log("Boomi server URL is missing. It is mandatory.")
		return
	}

	accountID := config.GlobalConfig.BoomiAcctId
	if accountID == "" {
		logger.Log("Boomi account ID is missing. It is mandatory.")
		return
	}

	boomiUserName := config.GlobalConfig.BoomiUser
	boomiPassword := config.GlobalConfig.BoomiPassword
	if boomiUserName == "" || boomiPassword == "" {
		logger.Log("Boomi username or password is missing.. It is mandatory..")
		return
	}

	boomiURL = strings.Replace(boomiURL, "{accountId}", accountID, 1)

	logger.Log("boomiURL: %s", boomiURL)
	logger.Log("accountId: %s", accountID)
	logger.Log("boomiUserName: %s", boomiUserName)

	output := BoomiExecutionOutput{pid: pid}
	outputFile, err := output.CreateFile()
	if err != nil {
		logger.Log(err.Error())
		return
	}
	defer output.CloseFile()

	executionRecords, err := fetchBoomiExecutionRecords(boomiUserName, boomiPassword, boomiURL)
	if err != nil {
		logger.Log(err.Error())
		return
	}

	if len(executionRecords) == 0 {
		logger.Log("No Boomi records to match the given criteria...")
		return
	}

	output.WriteHeader()
	output.WriteRecords(executionRecords)

	stats := NewExecutionRecordStats()
	stats.CalculateStats(executionRecords)
	stats.LogSummary()

	logger.Log("Finished capturing Boomi details, uploading to server")

	//// gets atom details
	atomQueryResult := getAtomQueryDetails(accountID, boomiUserName, boomiPassword)

	//// write atom details header
	output.WriteAtomDetailsHeader()
	/// write atom query details
	output.WriteAtomQueryDetails(atomQueryResult)

	///// get atom connector details
	atomConnectorURL := "https://api.boomi.com/api/rest/v1/" + accountID + "/Connector/query"
	atomConnectorRecord, err := fetchAtomConnectorDetails(boomiUserName, boomiPassword, atomConnectorURL)

	//// download atom log
	downloadAtomLog(boomiUserName, boomiPassword, accountID)

	//// write atom details header
	output.WriteAtomConnectorDetailsHeader()
	output.WriteAtomConnectorDetails(atomConnectorRecord)

	uploadBoomiDetailsToServer(endpoint, outputFile, "boomi")
}

func fetchBoomiExecutionRecords(boomiUserName, boomiPassword, boomiURL string) ([]ExecutionRecord, error) {
	totalRecordCount := 0
	stopped := false
	records := []ExecutionRecord{}

	queryToken := ""
	for {
		resp, err := makeBoomiRequest(queryToken, boomiUserName, boomiPassword, boomiURL)

		if err != nil {
			return records, fmt.Errorf("Failed to make Boomi request: %w", err)
		}
		logger.Log("Response Status Code: %d", resp.StatusCode())

		// return if status code is not 200
		if resp.StatusCode() != 200 {
			logger.Log("Boomi API responded with non 200, aborting...")
			return records, nil
		}

		// unmarshal the JSON response into the struct
		var queryResult BoomiExecutionRecordQueryResult
		jsonErr := json.Unmarshal(resp.Body(), &queryResult)
		if jsonErr != nil {
			return records, fmt.Errorf("Error unmarshalling Boomi response as JSON: %w", jsonErr)
		}

		logger.Log("Length of Boomi queryResult.Result->%d", len(queryResult.Result))

		if len(queryResult.Result) <= 0 {
			return records, nil
		}

		if len(queryResult.Result) > 0 {
			for _, record := range queryResult.Result {
				records = append(records, record)
				totalRecordCount++

				if totalRecordCount >= 10000 || totalRecordCount >= queryResult.NumberOfResults {
					stopped = true
					break
				}
			}
		}

		if stopped {
			logger.Log("Processed %d Boomi records", totalRecordCount-1)
			break
		}

		// assign query token from the current response
		queryToken = queryResult.QueryToken
	}

	return records, nil
}

func fetchAtomConnectorDetails(boomiUserName, boomiPassword, boomiURL string) ([]AtomConnector, error) {
	records := []AtomConnector{}
	totalRecordCount := 0
	stopped := false
	queryToken := ""
	for {
		resp, err := makeAtomConnectorsRequest(queryToken, boomiUserName, boomiPassword, boomiURL)

		if err != nil {
			//return fmt.Errorf("Failed to make Boomi request: %w", err)
		}
		logger.Log("Response Status Code: %d", resp.StatusCode())

		// return if status code is not 200
		if resp.StatusCode() != 200 {
			logger.Log("Boomi API responded with non 200, aborting...")
			return records, nil
		}

		// unmarshal the JSON response into the struct
		var queryResult AtomConnectorRecordQueryResult
		jsonErr := json.Unmarshal(resp.Body(), &queryResult)
		if jsonErr != nil {
			return records, fmt.Errorf("Error unmarshalling Boomi response as JSON: %w", jsonErr)
		}
		logger.Log("Length of Boomi queryResult.Result->%d", len(queryResult.Result))
		if len(queryResult.Result) <= 0 {
			return records, nil
		}

		if len(queryResult.Result) > 0 {
			for _, record := range queryResult.Result {
				records = append(records, record)
				totalRecordCount++

				if totalRecordCount >= queryResult.NumberOfResults {
					stopped = true
					break
				}
			}
		}

		if stopped {
			logger.Log("Processed %d Atom connector records", totalRecordCount-1)
			break
		}

		// assign query token from the current response
		queryToken = queryResult.QueryToken

	}

	return records, nil
}

type ExecutionRecordStats struct {
	RecordCount        int
	CountByStatus      map[string]int
	ExecutionTimeAvg   int
	ExecutionTimeTotal int
}

func NewExecutionRecordStats() *ExecutionRecordStats {
	return &ExecutionRecordStats{CountByStatus: make(map[string]int)}
}

func (ers *ExecutionRecordStats) CalculateStats(records []ExecutionRecord) {
	for _, executionRecord := range records {
		ers.CountByStatus[executionRecord.Status]++

		executionDuration := convertExecutionDurationToInt(executionRecord, executionRecord.Status)
		ers.ExecutionTimeTotal += executionDuration
	}

	ers.RecordCount = len(records)
	ers.ExecutionTimeAvg = ers.ExecutionTimeTotal / ers.RecordCount
}

func (ers *ExecutionRecordStats) LogSummary() {
	logger.Log("===================== BOOMI execution summary =====================")
	logger.Log("number of records: %d", ers.RecordCount)

	successJobCount, exist := ers.CountByStatus["COMPLETE"]
	if exist {
		logger.Log("number of SUCCESS: %d", successJobCount)
	}

	failedJobCount, exist := ers.CountByStatus["ERROR"]
	if exist {
		logger.Log("number of FAILURE: %d", failedJobCount)
	}

	executionTimeTotalMin := ers.ExecutionTimeTotal / 60000
	logger.Log("execution time total: %d min\n", executionTimeTotalMin)

	logger.Log("Avg execution time: %d ms\n", ers.ExecutionTimeAvg)
}

type BoomiExecutionOutput struct {
	pid  int
	file *os.File
}

func (b *BoomiExecutionOutput) CreateFile() (*os.File, error) {
	file, err := os.Create("boomi.out")
	if err != nil {
		return nil, fmt.Errorf("Error while creating Boomi output file: %w", err)
	}

	b.file = file

	return file, nil
}

func (b *BoomiExecutionOutput) CloseFile() error {
	if b.file == nil {
		return nil
	}

	return b.file.Close()
}

func (b *BoomiExecutionOutput) WriteHeader() error {
	if b.file == nil {
		return nil
	}

	// add boomi.out header
	boomiHeader := fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s, %s\n", "exec_time", "exec_duration", "status", "atom_name", "atom_id", "process_id", "atom_process_name", "execution_id")
	_, err := b.file.WriteString(boomiHeader)

	return err
}

func (b *BoomiExecutionOutput) WriteAtomDetailsHeader() error {
	if b.file == nil {
		return nil
	}

	// append delimiter string
	_, err := b.file.WriteString(";;;;;;;;\n")
	if err != nil {
		return err
	}
	// add boomi.out header

	atomDetailsHeader := fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n", "type", "name", "status", "atom_type", "host_name", "date_installed", "version", "atom_force_resatrt", "atom_id", "config")
	_, err2 := b.file.WriteString(atomDetailsHeader)

	return err2
}

func (b *BoomiExecutionOutput) WriteAtomConnectorDetailsHeader() error {
	if b.file == nil {
		return nil
	}

	// append delimiter string
	_, err := b.file.WriteString(";;;;;;;;\n")
	if err != nil {
		return err
	}
	// add boomi.out header

	atomConnectorDetailsHeader := fmt.Sprintf("%s,%s,%s\n", "type", "name", "atom_type")
	_, err2 := b.file.WriteString(atomConnectorDetailsHeader)

	return err2
}

func (b *BoomiExecutionOutput) WriteRecords(records []ExecutionRecord) error {
	if b.file == nil {
		return nil
	}

	for _, executionRecord := range records {
		executionDuration := convertExecutionDurationToInt(executionRecord, executionRecord.Status)

		boomiData := fmt.Sprintf("%s,%d,%s,%s,%s,%d,%s,%s\n", executionRecord.ExecutionTime, executionDuration, executionRecord.Status, executionRecord.AtomName, executionRecord.AtomID, b.pid, executionRecord.ProcessName, executionRecord.ExecutionID)
		_, err := b.file.WriteString(boomiData)

		if err != nil {
			return fmt.Errorf("error while writing boomi execution output: %w", err)
		}
	}

	err := b.file.Sync()
	if err != nil {
		return fmt.Errorf("error while file-sync'ing boomi execution output: %w", err)
	}

	return nil
}

func (b *BoomiExecutionOutput) WriteAtomQueryDetails(atomQueryResult AtomQueryResult) error {
	if b.file == nil {
		return nil
	}

	// get configured atom id
	atomId := config.GlobalConfig.AtomId
	logger.Log("configured atom id %s->", atomId)

	for _, atomQuery := range atomQueryResult.Result {
		if atomId == atomQuery.ID {
			boomiData := fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s,%d,%s,%s\n", atomQuery.Type, atomQuery.Name, atomQuery.Status, atomQuery.AtomType, atomQuery.HostName, atomQuery.DateInstalled, atomQuery.CurrentVersion, atomQuery.ForceRestartTime, atomQuery.ID, "Y")
			_, err := b.file.WriteString(boomiData)

			if err != nil {
				return fmt.Errorf("error while writing boomi execution output: %w", err)
			}
			//break
		} else {
			boomiData := fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s,%d,%s,%s\n", atomQuery.Type, atomQuery.Name, atomQuery.Status, atomQuery.AtomType, atomQuery.HostName, atomQuery.DateInstalled, atomQuery.CurrentVersion, atomQuery.ForceRestartTime, atomQuery.ID, "N")
			_, err := b.file.WriteString(boomiData)

			if err != nil {
				return fmt.Errorf("error while writing boomi execution output: %w", err)
			}
		}
	}

	err := b.file.Sync()
	if err != nil {
		return fmt.Errorf("error while file-sync'ing boomi execution output: %w", err)
	}

	return nil
}

func (b *BoomiExecutionOutput) WriteAtomConnectorDetails(atomConnectors []AtomConnector) error {
	if b.file == nil {
		return nil
	}

	for _, atomConnector := range atomConnectors {

		boomiData := fmt.Sprintf("%s,%s,%s\n", atomConnector.Type, atomConnector.Name, atomConnector.AtomType)
		_, err := b.file.WriteString(boomiData)

		if err != nil {
			return fmt.Errorf("error while writing boomi execution output: %w", err)
		}
	}

	err := b.file.Sync()
	if err != nil {
		return fmt.Errorf("error while file-sync'ing boomi execution output: %w", err)
	}

	return nil
}

// convert execution duration to integer
func convertExecutionDurationToInt(record ExecutionRecord, jobStatus string) int {
	if (jobStatus == "COMPLETE" || jobStatus == "ERROR") && len(record.ExecutionDuration) == 2 {
		if value, ok := record.ExecutionDuration[1].(float64); ok {
			executionDuration := int(value)
			return executionDuration
		}

		logger.Log("ExecutionDuration value is not a float64")
	} else {
		logger.Log("Unexpected format for ExecutionDuration")
	}

	return 0
}

// This method perform a BOOMI API POST request based on the query token value
// If the query token is NOT empty, it will hit queryMore URL with the query token
// received from the previous request and finally return the response
func makeBoomiRequest(queryToken string, username string, password string, boomiURL string) (*resty.Response, error) {

	// Create a new Resty client
	client := resty.New()

	// Set a hook to log the request
	client.OnBeforeRequest(func(c *resty.Client, req *resty.Request) error {

		// Log the request body, if any
		if req.Body != nil {
			// We need to handle different types of body data
			var bodyBytes []byte
			switch v := req.Body.(type) {
			case []byte:
				bodyBytes = v
			case string:
				bodyBytes = []byte(v)
			case *bytes.Buffer:
				bodyBytes = v.Bytes()
			default:
				// For other types, you might need to handle them differently or return an error
				fmt.Println("Request Body is of unsupported type")
				return nil
			}

			// Print the body content
			//fmt.Printf("Request Body: %s\n", string(bodyBytes))

			// Reset the request body
			req.Body = ioutil.NopCloser(bytes.NewReader(bodyBytes))
		}

		return nil
	})

	var period = config.GlobalConfig.Period
	if period == 0 {
		period = 3
	}

	logger.Log("current time frame %d hours", period)

	startTimeStr, endTimeStr := getStartAndEndTime(period)

	logger.Log("start time%s", startTimeStr)
	logger.Log("end time%s", endTimeStr)

	type FilterData struct {
		StartTimeStr string
		EndTimeStr   string
		AtomId       string
	}

	p := `{
		   "QueryFilter": {
					"expression": {
							"operator": "and",
							"nestedExpression": [
								{
									"operator": "BETWEEN",
                    				"property": "executionTime",
                    				"argument": ["{{.StartTimeStr}}", "{{.EndTimeStr}}"]
								},
								{
                     			   "argument" : ["{{.AtomId}}"],
                        			"operator":"EQUALS",
                        			"property":"atomId"
                    			}
							]
					}
			}
		}`

	var result bytes.Buffer
	atomId := config.GlobalConfig.AtomId
	if atomId != "" {
		data := FilterData{
			StartTimeStr: startTimeStr,
			EndTimeStr:   endTimeStr,
			AtomId:       atomId,
		}

		t, err := template.New("filter").Parse(p)
		if err != nil {
			logger.Log("error while parsing the boomi request template string %s", err.Error())
		}

		err = t.Execute(&result, data)
		if err != nil {
			logger.Log("error while applying template with value %s", err.Error())
		}
	}

	// query token empty,so will use boomi server default url
	if queryToken == "" {
		return client.R().
			SetBasicAuth(username, password).
			SetHeader(BoomiRequestAccept, BoomiRequestApplicationJSON).
			SetBody(result.String()).
			Post(boomiURL)
	}

	// queryMore scenario
	return client.R().
		SetBasicAuth(username, password).
		SetHeader(BoomiRequestContentType, BoomiRequestApplicationJSON).
		SetBody(queryToken).
		Post(boomiURL + "More")
}

// This method perform a BOOMI ATOM CONNECTORS POST request based on the query token value
// If the query token is NOT empty, it will hit queryMore URL with the query token
// received from the previous request and finally return the response
func makeAtomConnectorsRequest(queryToken string, username string, password string, boomiURL string) (*resty.Response, error) {

	// Create a new Resty client
	client := resty.New()

	// Set a hook to log the request
	client.OnBeforeRequest(func(c *resty.Client, req *resty.Request) error {

		// Log the request body, if any
		if req.Body != nil {
			// We need to handle different types of body data
			var bodyBytes []byte
			switch v := req.Body.(type) {
			case []byte:
				bodyBytes = v
			case string:
				bodyBytes = []byte(v)
			case *bytes.Buffer:
				bodyBytes = v.Bytes()
			default:
				// For other types, you might need to handle them differently or return an error
				fmt.Println("Request Body is of unsupported type")
				return nil
			}

			// Print the body content
			//fmt.Printf("Request Body: %s\n", string(bodyBytes))

			// Reset the request body
			req.Body = ioutil.NopCloser(bytes.NewReader(bodyBytes))
		}

		return nil
	})

	// query token empty,so will use boomi server default url
	if queryToken == "" {
		return client.R().
			SetBasicAuth(username, password).
			SetHeader(BoomiRequestAccept, BoomiRequestApplicationJSON).
			Post(boomiURL)
	}

	// queryMore scenario
	return client.R().
		SetBasicAuth(username, password).
		SetHeader(BoomiRequestContentType, BoomiRequestApplicationJSON).
		SetBody(queryToken).
		Post(boomiURL + "More")
}

// Upload data to the YC server
// endpoint, podName, file, cmdType
func uploadBoomiDetailsToServer(endpoint string, dataFile *os.File, paramType string) {
	// upload to server
	msg, ok := PostCustomDataWithPositionFunc(endpoint, fmt.Sprintf("dt=%s", paramType), dataFile, PositionLast5000Lines)
	fmt.Println(msg)
	fmt.Println(ok)
}

func getStartAndEndTime(period uint) (string, string) {
	// get current time
	currentTime := time.Now().UTC()

	duration := time.Duration(period) * time.Hour

	// Subtract one hour from the current time
	startTime := currentTime.Add(-duration)

	// Define the end time as the current time
	endTime := currentTime

	// Format the times to the required string format
	startTimeStr := startTime.Format(time.RFC3339)
	endTimeStr := endTime.Format(time.RFC3339)
	return startTimeStr, endTimeStr
}

func getAtomQueryDetails(accountID string, username string, password string) AtomQueryResult {
	atomURL := "https://api.boomi.com/api/rest/v1/" + accountID + "/Atom/query"

	// Create a new Resty client
	client := resty.New()

	resp, err := client.R().
		SetBasicAuth(username, password).
		SetHeader(BoomiRequestAccept, BoomiRequestApplicationJSON).
		Post(atomURL)

	if err != nil {
		logger.Log("error while calling atom query details rest endpoint %s", err.Error())
	}

	logger.Log("atom query result status code %d", resp.StatusCode())

	var atomQueryResult AtomQueryResult

	// return if status code is not 200
	if resp.StatusCode() != 200 {
		logger.Log("Boomi Atom details api responded with non 200, aborting...")
		return atomQueryResult
	}

	jsonErr := json.Unmarshal(resp.Body(), &atomQueryResult)
	if jsonErr != nil {
		logger.Log("error while unmarshalling response %s", jsonErr.Error())
		return atomQueryResult
	}

	//logger.Log("Atom Query details Result %v", atomQueryResult)

	return atomQueryResult
}

func getAtomConnectorDetails(accountID string, username string, password string) {
	// Create a new Resty client
	client := resty.New()
	connectorURL := "https://api.boomi.com/api/rest/v1/" + accountID + "/Connector/query"
	resp, err := client.R().
		SetBasicAuth(username, password).
		SetHeader(BoomiRequestAccept, BoomiRequestApplicationJSON).
		Post(connectorURL)

	if err != nil {
		logger.Log("error while calling atom connector details rest endpoint %s", err.Error())
	}

	logger.Log("atom connector result status code %d", resp.StatusCode())

}

// use the following URL to download the container id
// https://api.boomi.com/mdm/api/rest/v1/<account_id/clouds
// this will return a similar response like this
// <mdm:Clouds>
// <mdm:Cloud cloudId="47ff4c06-8bef-431e-a30f-2f4dec0ffca8" containerId="acd927c3-a249-4a47-b217-ef9cbf99d187" name="Singapore Hub Cloud"/>
// </mdm:Clouds>
func downloadAtomLog(username, password, boomiAcctId string) {
	logger.Log("now downloading atom log..")
	var period = config.GlobalConfig.Period
	if period == 0 {
		period = 3
	}
	_, endTimeStr := getStartAndEndTime(period)

	atomId := config.GlobalConfig.AtomId

	req := `{
		"atomId": "{{.AtomId}}",
		"logDate":"{{.LogDate}}"
	}`

	type FilterData struct {
		LogDate string
		AtomId  string
	}

	var result bytes.Buffer
	data := FilterData{
		LogDate: endTimeStr,
		AtomId:  atomId,
	}

	t, err := template.New("filter").Parse(req)
	if err != nil {
		logger.Log("error while parsing the boomi download atom request template string %s", err.Error())
	}
	err = t.Execute(&result, data)
	if err != nil {
		logger.Log("error while applying template with value %s", err.Error())
	}

	client := resty.New()
	boomiURL := "https://api.boomi.com/api/rest/v1/" + boomiAcctId + "/AtomLog"
	logger.Log("boomi atom log req string %s", result.String())

	resp, err := client.R().
		SetBasicAuth(username, password).
		SetHeader(BoomiRequestContentType, BoomiRequestApplicationJSON).
		SetBody(result.String()).
		Post(boomiURL)

	logger.Log("boomi atom download log response code %d", resp.StatusCode())
	if resp.StatusCode() != 202 {
		logger.Log("Boomi API responded with non 202, aborting...")
		return
	}

	type BoomiAtomLogQueryResult struct {
		Type       string `json:"@type"`
		Url        string `json:"url"`
		Message    string `json:"message"`
		StatusCode int    `json:"statusCode"`
	}

	// unmarshal the JSON response into the struct
	var queryResult BoomiAtomLogQueryResult
	jsonErr := json.Unmarshal(resp.Body(), &queryResult)
	if jsonErr != nil {
		logger.Log("Error unmarshalling Boomi atom download log response as JSON: %w", jsonErr)
	}
	logger.Log("got atom log download url->%s", queryResult.Url)

	// client2 := resty.New()
	// client2.SetDebug(true)

	// // Set a custom User-Agent to mimic a real browser
	// client2.SetHeader("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/58.0.3029.110 Safari/537.36")

	// // Create the HTTP client

	// // Open the file to write the content
	// outFile, err := os.Create(atomId + ".zip")
	// if err != nil {
	// 	fmt.Println("Error creating the file:", err)
	// 	return
	// }
	// defer outFile.Close()

	if queryResult.Url != "" {
		// 	// got the download url, now download the log file
		_, err := client.R().
			SetBasicAuth(username, password).
			SetOutput(atomId).
			Get(queryResult.Url)

		if err != nil {
			logger.Log("error while get operation in boomi atom log download.. aborting")
			return
		}

		// 	logger.Log("boomi atom download url response code-> %d", resp.StatusCode())
		// 	if resp.StatusCode() != 202 {
		// 		logger.Log("error while downloading boomi atom log.. aborting")
		// 		return
		// 	}

		// client := &http.Client{}
		// req, err := http.NewRequest("POST", queryResult.Url, nil)
		// if err != nil {
		// 	fmt.Println("Error creating the request:", err)
		// 	return
		// }
		// auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		// req.Header.Set("Authorization", "Basic "+auth)
		// resp, err := client.Do(req)
		// if err != nil {
		// 	fmt.Println("Error sending the request:", err)
		// 	return
		// }
		// defer resp.Body.Close()
		// outFile, err := os.Create("downloaded-file.zip")
		// if err != nil {
		// 	fmt.Println("Error creating the file:", err)
		// 	return
		// }
		// defer outFile.Close()
		// _, err = io.Copy(outFile, resp.Body)
		// if err != nil {
		// 	fmt.Println("Error writing to the file:", err)
		// 	return
		// }

		fmt.Println("File downloaded successfully ")
	}

}

// downloads the atom diskspace details from the Boomi server
func getAtomDiskSize(accountID string, atomId string, username string, password string, records []ExecutionRecord) {

	uniqueData := make(map[string]struct{})
	// Create a slice to store unique values
	var atomIDResult []string
	// iterate through all the execution records and store the atom id
	for _, executionRecord := range records {
		atomID := executionRecord.AtomID
		if _, exists := uniqueData[atomID]; !exists {
			uniqueData[atomID] = struct{}{}             // Add to map
			atomIDResult = append(atomIDResult, atomID) // Add to slice
		}
	}

	logger.Log("atomIDResult->%s", atomIDResult)
	// Create a new Resty client
	client := resty.New()

	/// iterate through iterate through the atomIDResult and download the atom diskspace information
	var atomURL string
	for _, atmID := range atomIDResult {
		atomURL = "https://api.boomi.com/api/rest/v1/" + accountID + "/async/AtomDiskSpace/"
		resp, err := client.R().
			SetBasicAuth(username, password).
			SetHeader(BoomiRequestAccept, BoomiRequestApplicationJSON).
			SetBody(atmID).
			Post(atomURL)

		if err != nil {
			logger.Log("error while calling atom asycn rest endpoint %s", err.Error())
		}

		logger.Log("atom disk space status code %d", resp.StatusCode())
		// return if status code is not 200
		if resp.StatusCode() != 202 {
			logger.Log("Boomi API responded with non 202, aborting...")
			return
		}

		// unmarshal the JSON response into the struct
		var asyncToken AsyncToken
		jsonErr := json.Unmarshal(resp.Body(), &asyncToken)
		if jsonErr != nil {
			return
		}
		logger.Log("atom async response->%s", jsonErr)

		/// now call the atom disk space rest endpoint
		if asyncToken.Token != "" {
			atomURL = "https://api.boomi.com/api/rest/v1/" + accountID + "/async/AtomDiskSpace/response/" + asyncToken.Token

			resp, err := client.R().
				SetBasicAuth(username, password).
				SetHeader(BoomiRequestAccept, BoomiRequestApplicationJSON).
				Get(atomURL)

			if err != nil {
				logger.Log("error while applying template with value %s", err.Error())
			}

			// return if status code is not 200
			if resp.StatusCode() != 200 {
				logger.Log("Boomi API responded with non 200, aborting...")
				return
			}

			var asyncAtomDiskspaceTokenResult AsyncAtomDiskspaceTokenResult
			jsonErr := json.Unmarshal(resp.Body(), &asyncAtomDiskspaceTokenResult)
			if jsonErr != nil {
				logger.Log("error while unmarshalling response %s", jsonErr.Error())
				return
			}
			logger.Log("asyncAtomDiskspaceTokenResult %v", asyncAtomDiskspaceTokenResult)
		}

	}

}
