package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// only one thread can run capture process
var one sync.Mutex

type Server struct {
	*http.Server
	ProcessPids func(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error)
	ln          net.Listener
}

type Req struct {
	Key     string
	Actions []string
	WaitFor bool
	Hd      *bool
	Tags    string
}

type Resp struct {
	Code                int
	Msg                 string
	DashboardReportURLs []string   `json:",omitempty"`
	Output              [][]string `json:",omitempty"`
}

func NewServer(address string, port int) (s *Server, err error) {
	addr := net.JoinHostPort(address, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}
	mux := http.NewServeMux()
	s = &Server{
		Server: &http.Server{
			Handler: mux,
		},
		ProcessPids: ProcessPidsWithMutex,
		ln:          ln,
	}
	mux.HandleFunc("/action", s.Action)
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

func (s *Server) Serve() error {
	return s.Server.Serve(s.ln)
}

func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

func (s *Server) Action(writer http.ResponseWriter, request *http.Request) {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	resp := &Resp{}
	var err error
	defer func() {
		err = encoder.Encode(resp)
		if err != nil {
			logger.Log("failed to encode response(%#v): %v", resp, err)
		}
	}()

	forward := request.Header.Get("ycrash-forward")
	if len(forward) > 0 {
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
		client := http.Client{}
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
		return
	}

	decoder := json.NewDecoder(request.Body)
	req := &Req{}
	err = decoder.Decode(req)
	if err != nil {
		resp.Code = -1
		resp.Msg = err.Error()
		return
	}
	logger.Info().Msgf("action request: %#v", req)

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
				resp.Output = append(resp.Output, []string{s})
			}
		}

		return
	}

	// Validate at least 1 pid exists
	var pids []int
	for _, i := range result {
		if pid, ok := i.(int); ok {
			pids = append(pids, pid)
		}
	}

	atLeast1PidExist := false
	for _, pid := range pids {
		if capture.IsProcessExists(pid) {
			atLeast1PidExist = true
			break
		}
	}

	if !atLeast1PidExist {
		resp.Code = -1
		resp.Msg = "You have entered non-existent process ids."
		return
	}

	var needHeapDump bool
	if req.Hd != nil {
		needHeapDump = *req.Hd
	} else {
		logger.Info().Msg("no hd passed in the request, using global config")
		needHeapDump = config.GlobalConfig.HeapDump
	}
	if req.WaitFor || hasCmd {
		var rUrls []string
		if !hasCmd {
			rUrls, err = s.ProcessPids(pids, pid2Name, needHeapDump, req.Tags)
			if err != nil {
				resp.Code = -1
				resp.Msg = err.Error()
				return
			}
			resp.DashboardReportURLs = rUrls
		} else {
			var pid int
			_ = fmt.Sprintf("%d", pid) // Blank identifier to indicate intentional unused variable
			for _, i := range result {
				var output []string
				if p, ok := i.(int); ok {
					pid = p
					output = append(output, strconv.Itoa(p))
					rUrls, err = s.ProcessPids([]int{p}, pid2Name, needHeapDump, req.Tags)
					if err == nil {
						resp.DashboardReportURLs = append(resp.DashboardReportURLs, rUrls...)
						output = append(output, rUrls...)
					} else {
						output = append(output, err.Error())
					}
				} else if cmd, ok := i.(string); ok {
					output = append(output, cmd)
					// Display "Unsupported Operation" message
					if len(output) == 1 {
						output = []string{"Unsupported Operation"}
					}
				}
				resp.Output = append(resp.Output, output)
			}
		}
		return
	}
	if !hasCmd {
		go func() {
			_, err := s.ProcessPids(pids, pid2Name, needHeapDump, req.Tags)
			if err != nil {
				logger.Log("failed to process pids in background: %v", err)
			}
		}()
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
