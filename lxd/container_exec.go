package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/lxc/lxd/shared"
	"gopkg.in/lxc/go-lxc.v2"
)

func runCommand(container *lxc.Container, command []string, options lxc.AttachOptions) shared.OperationResult {
	status, err := container.RunCommandStatus(command, options)
	if err != nil {
		shared.Debugf("Failed running command: %q", err.Error())
		return shared.OperationError(err)
	}

	metadata, err := json.Marshal(shared.Jmap{"return": status})
	if err != nil {
		return shared.OperationError(err)
	}

	return shared.OperationResult{Metadata: metadata, Error: nil}
}

func (s *execWs) Metadata() interface{} {
	fds := shared.Jmap{}
	for fd, secret := range s.fds {
		if fd == -1 {
			fds["control"] = secret
		} else {
			fds[strconv.Itoa(fd)] = secret
		}
	}

	return shared.Jmap{"fds": fds}
}

func (s *execWs) Connect(secret string, r *http.Request, w http.ResponseWriter) error {
	for fd, fdSecret := range s.fds {
		if secret == fdSecret {
			conn, err := shared.WebsocketUpgrader.Upgrade(w, r, nil)
			if err != nil {
				return err
			}

			s.conns[fd] = conn

			if fd == -1 {
				s.controlConnected <- true
				return nil
			}

			for i, c := range s.conns {
				if i != -1 && c == nil {
					return nil
				}
			}
			s.allConnected <- true
			return nil
		}
	}

	/* If we didn't find the right secret, the user provided a bad one,
	 * which 403, not 404, since this operation actually exists */
	return os.ErrPermission
}

func (s *execWs) Do() shared.OperationResult {
	<-s.allConnected

	var err error
	var ttys []*os.File
	var ptys []*os.File

	if s.interactive {
		ttys = make([]*os.File, 1)
		ptys = make([]*os.File, 1)
		ptys[0], ttys[0], err = shared.OpenPty(s.rootUid, s.rootGid)
		s.options.StdinFd = ttys[0].Fd()
		s.options.StdoutFd = ttys[0].Fd()
		s.options.StderrFd = ttys[0].Fd()
	} else {
		ttys = make([]*os.File, 3)
		ptys = make([]*os.File, 3)
		for i := 0; i < len(ttys); i++ {
			ptys[i], ttys[i], err = shared.Pipe()
			if err != nil {
				return shared.OperationError(err)
			}
		}
		s.options.StdinFd = ptys[0].Fd()
		s.options.StdoutFd = ttys[1].Fd()
		s.options.StderrFd = ttys[2].Fd()
	}

	controlExit := make(chan bool)
	stdEOF := make(chan bool)

	if s.interactive {
		go func() {
			select {
			case <-s.controlConnected:
				break

			case <-controlExit:
				return
			}

			for {
				mt, r, err := s.conns[-1].NextReader()
				if mt == websocket.CloseMessage {
					break
				}

				if err != nil {
					shared.Debugf("got error getting next reader %s", err)
					break
				}

				buf, err := ioutil.ReadAll(r)
				if err != nil {
					shared.Debugf("failed to read message %s", err)
					break
				}

				command := shared.ContainerExecControl{}

				if err := json.Unmarshal(buf, &command); err != nil {
					shared.Debugf("failed to unmarshal control socket command: %s", err)
					continue
				}

				if command.Command == "window-resize" {
					winchWidth, err := strconv.Atoi(command.Args["width"])
					if err != nil {
						shared.Debugf("Unable to extract window width: %s", err)
						continue
					}

					winchHeight, err := strconv.Atoi(command.Args["height"])
					if err != nil {
						shared.Debugf("Unable to extract window height: %s", err)
						continue
					}

					err = shared.SetSize(int(ptys[0].Fd()), winchWidth, winchHeight)
					if err != nil {
						shared.Debugf("Failed to set window size to: %dx%d", winchWidth, winchHeight)
					}
				}

				if err != nil {
					shared.Debugf("got error writing to writer %s", err)
					break
				}
			}
		}()

		shared.WebsocketMirror(s.conns[0], ptys[0], ptys[0])
	} else {
		for i := 0; i < len(ttys); i++ {
			go func(i int) {
				if i == 0 {
					<-shared.WebsocketRecvStream(ttys[i], s.conns[i])
					ttys[i].Close()
				} else {
					<-shared.WebsocketSendStream(s.conns[i], ptys[i])
					ptys[i].Close()
					stdEOF <- true
				}
			}(i)
		}
	}

	result := runCommand(
		s.container,
		s.command,
		s.options,
	)

	if !s.interactive {
		ttys[0].Close()
		ttys[1].Close()
		ttys[2].Close()
		<-stdEOF
	}

	for _, tty := range ttys {
		tty.Close()
	}

	for _, pty := range ptys {
		pty.Close()
	}

	if s.interactive && s.conns[-1] == nil {
		controlExit <- true
	}

	return result
}

func containerExecPost(d *Daemon, r *http.Request) Response {
	name := mux.Vars(r)["name"]
	c, err := newLxdContainer(name, d)
	if err != nil {
		return SmartError(err)
	}

	if !c.c.Running() {
		return BadRequest(fmt.Errorf("Container is not running."))
	}

	post := commandPostContent{}
	buf, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return BadRequest(err)
	}

	if err := json.Unmarshal(buf, &post); err != nil {
		return BadRequest(err)
	}

	opts := lxc.DefaultAttachOptions
	opts.ClearEnv = true
	opts.Env = []string{}

	if post.Environment != nil {
		for k, v := range post.Environment {
			if k == "HOME" {
				opts.Cwd = v
			}
			opts.Env = append(opts.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	if post.WaitForWS {
		ws := &execWs{}
		ws.fds = map[int]string{}
		if c.idmapset != nil {
			ws.rootUid, ws.rootGid = c.idmapset.ShiftIntoNs(0, 0)
		}
		ws.conns = map[int]*websocket.Conn{}
		ws.conns[-1] = nil
		ws.conns[0] = nil
		if !post.Interactive {
			ws.conns[1] = nil
			ws.conns[2] = nil
		}
		ws.allConnected = make(chan bool, 1)
		ws.controlConnected = make(chan bool, 1)
		ws.interactive = post.Interactive
		ws.done = make(chan shared.OperationResult, 1)
		ws.options = opts
		for i := -1; i < len(ws.conns)-1; i++ {
			ws.fds[i], err = shared.RandomCryptoString()
			if err != nil {
				return InternalError(err)
			}
		}

		ws.command = post.Command
		ws.container = c.c

		return AsyncResponseWithWs(ws, nil)
	}

	run := func() shared.OperationResult {

		nullDev, err := os.OpenFile(os.DevNull, os.O_RDWR, 0666)
		if err != nil {
			return shared.OperationError(err)
		}
		defer nullDev.Close()
		nullfd := nullDev.Fd()

		opts.StdinFd = nullfd
		opts.StdoutFd = nullfd
		opts.StderrFd = nullfd

		return runCommand(c.c, post.Command, opts)
	}

	return AsyncResponse(run, nil)
}
