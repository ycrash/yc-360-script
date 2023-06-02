package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"shell/config"
	"shell/logger"
	"strconv"
	"strings"
)

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
			var pids []int
			for _, i := range result {
				pids = append(pids, i.(int))
			}
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
				} /* else if cmd, ok := i.(string); ok {
					output = append(output, cmd)
					out, err := RunCaptureCmd(pid, cmd)
					if err == nil {
						output = append(output, string(out))
					} else {
						output = append(output, err.Error())
					}
				} */
				resp.Output = append(resp.Output, output)
			}
		}
		return
	}
	if !hasCmd {
		var pids []int
		for _, i := range result {
			pids = append(pids, i.(int))
		}
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
					pid, err = GetTopCpu()
					if err != nil {
						return
					}
				case "PROCESS_HIGH_MEMORY":
					pid, err = GetTopMem()
					if err != nil {
						return
					}
				case "PROCESS_UNKNOWN":
					pid, err = GetTopCpu()
					if err != nil {
						return
					}
					if pid > 0 {
						result = append(result, pid)
					}
					pid, err = GetTopMem()
					if err != nil {
						return
					}
				default:
					var e error
					pid, e = strconv.Atoi(id)
					// "actions": ["capture buggyApp.jar"]
					if e != nil {
						p2n, e := GetProcessIds(config.ProcessTokens{config.ProcessToken(id)}, nil)
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
			msg, ok := attend("api")
			fmt.Printf(
				`api attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
		} /* else {
			hasCmd = true
			result = append(result, s)
		} */
	}
	return
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
		ln: ln,
	}
	mux.HandleFunc("/action", s.Action)
	return
}

func (s *Server) Serve() error {
	return s.Server.Serve(s.ln)
}

func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}
