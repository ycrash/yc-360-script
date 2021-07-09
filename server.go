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
	"strconv"
	"strings"
)

type Server struct {
	*http.Server
	ProcessPids func(pids []int) (rUrls []string, err error)
	ln          net.Listener
}

type Req struct {
	Key     string
	Actions []string
	WaitFor bool
}

type Resp struct {
	Code                int
	Msg                 string
	DashboardReportURLs []string `json:",omitempty"`
}

func (s *Server) Action(writer http.ResponseWriter, request *http.Request) {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	resp := &Resp{}
	defer func() {
		encoder.Encode(resp)
	}()

	forward := request.Header.Get("ycrash-forward")
	if len(forward) > 0 {
		fr := request.Clone(context.Background())
		url, err := url.Parse(forward)
		if err != nil {
			resp.Code = -1
			resp.Msg = err.Error()
			return
		}
		fr.RequestURI = ""
		fr.URL = url
		fr.Header.Del("ycrash-forward")
		fr.Close = true
		client := http.Client{}
		r, err := client.Do(fr)
		if err != nil {
			resp.Code = -2
			resp.Msg = err.Error()
			return
		}
		defer r.Body.Close()
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
	err := decoder.Decode(req)
	if err != nil {
		resp.Code = -1
		resp.Msg = err.Error()
		return
	}

	if config.GlobalConfig.ApiKey != req.Key {
		resp.Code = -1
		resp.Msg = "invalid key passed"
		return
	}

	pids, err := parseActions(req.Actions)
	if err != nil {
		resp.Code = -1
		resp.Msg = err.Error()
		return
	}

	if req.WaitFor {
		rUrls, err := s.ProcessPids(pids)
		if err != nil {
			resp.Code = -1
			resp.Msg = err.Error()
			return
		}
		resp.DashboardReportURLs = rUrls
		return
	}
	go s.ProcessPids(pids)
}

func parseActions(actions []string) (pids []int, err error) {
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
						pids = append(pids, pid)
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
						var ids []int
						ids, e = GetProcessIds(config.ProcessTokens{config.ProcessToken(id)})
						if e != nil {
							continue
						}
						for _, pid := range ids {
							if pid > 0 {
								pids = append(pids, pid)
							}
						}
						continue
					}
				}
				if pid > 0 {
					pids = append(pids, pid)
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
		}
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
