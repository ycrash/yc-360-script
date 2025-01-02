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
	boomiHeader := fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s\n", "exec_time", "exec_duration", "status", "atom_name", "atom_id", "process_id", "atom_process_name")
	_, err := b.file.WriteString(boomiHeader)

	return err
}

func (b *BoomiExecutionOutput) WriteRecords(records []ExecutionRecord) error {
	if b.file == nil {
		return nil
	}

	for _, executionRecord := range records {
		executionDuration := convertExecutionDurationToInt(executionRecord, executionRecord.Status)

		boomiData := fmt.Sprintf("%s,%d,%s,%s,%s,%d,%s\n", executionRecord.ExecutionTime, executionDuration, executionRecord.Status, executionRecord.AtomName, executionRecord.AtomID, b.pid, executionRecord.ProcessName)
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
			fmt.Printf("Request Body: %s\n", string(bodyBytes))

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
