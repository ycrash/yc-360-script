package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"yc-agent/internal/agent/common"
	"yc-agent/internal/agent/ondemand"
	"yc-agent/internal/capture"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// only one goroutine can run the capture process
var one sync.Mutex

type ActionRequest struct {
	Key     string
	Actions []string
	WaitFor bool
	Hd      *bool
	Tags    string
}

type ActionResponse struct {
	Code                int
	Msg                 string
	DashboardReportURLs []string   `json:",omitempty"`
	Output              [][]string `json:",omitempty"`
}

func (s *Server) Action(writer http.ResponseWriter, request *http.Request) {
	resp := &ActionResponse{}

	if request.Header.Get("ycrash-forward") != "" {
		s.handleYcrashForward(writer, request, resp)
	} else {
		// Decode request
		req := &ActionRequest{}
		decoder := json.NewDecoder(request.Body)
		err := decoder.Decode(req)
		if err != nil {
			resp.Code = -1
			resp.Msg = err.Error()
		} else {
			logger.Info().Msgf("action request: %#v", req)
			s.handleActionAPI(req, resp)
		}
	}

	// Send response
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)

	err := encoder.Encode(resp)
	if err != nil {
		logger.Log("failed to encode response(%#v): %v", resp, err)
	}
}

func (s *Server) handleActionAPI(req *ActionRequest, resp *ActionResponse) {
	if config.GlobalConfig.ApiKey != req.Key {
		resp.Code = -1
		resp.Msg = "invalid key passed"
		return
	}

	result, pid2Name, hasCmd, err := parseActions(req.Actions)
	if err != nil {
		resp.Code = -1
		resp.Msg = err.Error()
		return
	}

	resultValid, respMsg, respOutput := s.validateActionAPIParseResult(result)
	if !resultValid {
		resp.Code = -1
		resp.Msg = respMsg
		resp.Output = append(resp.Output, respOutput...)
		return
	}

	var needHeapDump bool
	if req.Hd != nil {
		needHeapDump = *req.Hd
	} else {
		logger.Info().Msg("no hd passed in the request, using global config")
		needHeapDump = config.GlobalConfig.HeapDump
	}

	shouldWait := req.WaitFor
	if hasCmd {
		// Legacy behavior, not sure why.
		// Probably need to include "Unsupported Operation" in the response.
		shouldWait = true
	}

	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()

		// temporary / scoped to this func
		type processPidsResult struct {
			rUrls  []string
			err    error
			output []string
		}

		processPidsResults := map[any]processPidsResult{}
		for _, pidAny := range result {
			pResult := processPidsResult{}

			if pid, ok := pidAny.(int); ok {
				pResult.output = append(pResult.output, strconv.Itoa(pid))
				pResult.rUrls, pResult.err = s.ProcessPids([]int{pid}, pid2Name, needHeapDump, req.Tags)

				if pResult.err == nil {
					pResult.output = append(pResult.output, pResult.rUrls...)
				} else {
					pResult.output = append(pResult.output, err.Error())
				}
			} else if _, ok := pidAny.(string); ok {
				pResult.output = append(pResult.output, "Unsupported Operation")
			}

			processPidsResults[pidAny] = pResult
		}

		// Populate Resp
		if len(processPidsResults) == 1 {
			// Maintain legacy response for 1 pid
			for _, r := range processPidsResults {
				if r.err != nil {
					resp.Code = -1
					resp.Msg = err.Error()
				} else {
					resp.DashboardReportURLs = r.rUrls
				}
			}
		} else if len(processPidsResults) > 1 {
			for _, r := range processPidsResults {
				resp.DashboardReportURLs = append(resp.DashboardReportURLs, r.rUrls...)
				resp.Output = append(resp.Output, r.output)
			}
		}

		logger.Log("action api completed: %v", resp)
	}()

	if shouldWait {
		wg.Wait()
	}
}

func (s *Server) handleYcrashForward(writer http.ResponseWriter, request *http.Request, resp *ActionResponse) {
	var err error

	forward := request.Header.Get("ycrash-forward")
	fr := request.Clone(context.Background())
	fr.URL, err = url.Parse(forward)
	if err != nil {
		resp.Code = -1
		resp.Msg = err.Error()
		return
	}
	fr.RequestURI = ""
	fr.Header.Del("ycrash-forward")
	fr.Close = true
	client := http.Client{
		Timeout: config.GlobalConfig.HttpClientTimeout,
	}
	r, err := client.Do(fr)
	if err != nil {
		resp.Code = -2
		resp.Msg = err.Error()
		return
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			logger.Log("failed to close response body: %v", err)
		}
	}()
	for key, v := range r.Header {
		for _, value := range v {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(r.StatusCode)
	_, err = io.Copy(writer, r.Body)
	if err != nil {
		resp.Code = -1
		resp.Msg = err.Error()
		return
	}
}

func parseActions(actions []string) (result []interface{}, pid2Name map[int]string, hasCmd bool, err error) {
	for _, s := range actions {
		if strings.HasPrefix(s, "capture ") {
			ss := strings.Split(s, " ")
			if len(ss) == 2 {
				id := strings.TrimSpace(ss[1])
				var pid int
				switch id {
				case "PROCESS_HIGH_CPU":
					pid, err = capture.GetTopCpu()
					if err != nil {
						return
					}
				case "PROCESS_HIGH_MEMORY":
					pid, err = capture.GetTopMem()
					if err != nil {
						return
					}
				case "PROCESS_UNKNOWN":
					pid, err = capture.GetTopCpu()
					if err != nil {
						return
					}
					if pid > 0 {
						result = append(result, pid)
					}
					pid, err = capture.GetTopMem()
					if err != nil {
						return
					}
				default:
					var e error
					pid, e = strconv.Atoi(id)
					// "actions": ["capture buggyApp.jar"]
					if e != nil {
						p2n, e := capture.GetProcessIds(config.ProcessTokens{config.ProcessToken(id)}, nil)
						if e != nil {
							continue
						}
						for pid, name := range p2n {
							if pid > 0 {
								if pid2Name == nil {
									pid2Name = make(map[int]string, len(p2n))
								}
								result = append(result, pid)
								pid2Name[pid] = name
							}
						}
						continue
					}
				}
				if pid > 0 {
					result = append(result, pid)
				}
			}
		} else if s == "attendance" {
			msg, ok := common.AttendWithType("api")
			fmt.Printf(
				`api attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
		} else {
			hasCmd = true
			// Display "Unsupported Operation" message
			result = append(result, "Unsupported Operation")
		}
	}
	return
}

func (s *Server) validateActionAPIParseResult(result []interface{}) (valid bool, respMsg string, respOutput [][]string) {
	valid = true
	respOutput = [][]string{}

	{
		// Validate at least 1 action exists
		unsupportedOperationCount := 0
		for _, i := range result {
			if s, ok := i.(string); ok {
				if s == "Unsupported Operation" {
					unsupportedOperationCount++
				}
			}
		}

		// If all result is ["Unsupported Operation", "Unsupported Operation"]
		// We can't continue, since we have no supported operation
		// {"Code":0,"Msg":"","Output":[["Unsupported Operation"]]}
		if len(result) == unsupportedOperationCount {
			for _, i := range result {
				if s, ok := i.(string); ok {
					respOutput = append(respOutput, []string{s})
				}
			}

			valid = false
			return
		}
	}

	var pids []int
	for _, i := range result {
		if pid, ok := i.(int); ok {
			pids = append(pids, pid)
		}
	}

	// Validate at least 1 pid exists
	{
		atLeast1PidExist := false
		for _, pid := range pids {
			if capture.IsProcessExists(pid) {
				atLeast1PidExist = true
				break
			}
		}

		if !atLeast1PidExist {
			// resp.Code = -1
			respMsg = "You have entered non-existent process ids."
			valid = false
			return
		}
	}

	return
}

// ProcessPidsWithMutext runs ProcessPids, synchronized with mutex lock
// to allow only one function running at a time.
func ProcessPidsWithMutex(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error) {
	one.Lock()
	defer one.Unlock()

	tmp := config.GlobalConfig.Tags
	if len(tmp) > 0 {
		ts := strings.Trim(tmp, ",")
		tmp = strings.Trim(ts+","+tags, ",")
	} else {
		tmp = strings.Trim(tags, ",")
	}

	return ondemand.ProcessPids(pids, pid2Name, hd, tmp, []string{""})
}
