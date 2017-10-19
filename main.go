package main

import (
	"errors"
	"fmt"
	"golang.org/x/net/websocket"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hpcloud/tail"

	"github.com/comail/colog"
	"github.com/jessevdk/go-flags"
)

// -----------------------------------------------------------------------------

// Flags defines local application flags
type Flags struct {
	Addr        string `long:"http_addr"   default:":8080"          description:"Http listen address"`
	Host        string `long:"host"        default:""               description:"Hostname for page title"`
	LogLevel    string `long:"log_level"   default:"info"           description:"Log level [warn|info|debug]"`
	Root        string `long:"root"        default:"log/"           description:"Root directory for log files"`
	Back        int64  `long:"back"        default:"5000"           description:"tail from the last Nth location"`
	MaxLineSize int    `long:"split"       default:"180"            description:"min line size for split"`
	Poll        bool   `long:"p"           description:"use polling, instead of inotify"`
	Version     bool   `long:"version"     description:"Show version and exit"`
}

// Config holds all config vars
type Config struct {
	Flags
}

// FileAttr holds File Attrs
type FileAttr struct {
	MTime time.Time `json:"mtime"`
	Size  int64     `json:"size"`
}

// FileStore holds all log files attrs
type FileStore map[string]*FileAttr

// -----------------------------------------------------------------------------

type message struct {
	Channel string `json:"channel"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

type listmessage struct {
	Message FileStore `json:"message"`
	Error   string    `json:"error,omitempty"`
}

var (
	cfg Config
	lg  *log.Logger
)

// -----------------------------------------------------------------------------

func loadLogs() (files FileStore, err error) {
	files = FileStore{}
	dir := strings.TrimSuffix(cfg.Root, "/")
	err = filepath.Walk(cfg.Root, func(path string, f os.FileInfo, err error) error {
		if !f.IsDir() {
			p := strings.TrimPrefix(path, dir+"/")
			lg.Printf("debug: found logfile %s", p)
			files[p] = &FileAttr{MTime: f.ModTime(), Size: f.Size()}
		}
		return nil
	})
	return
}

// -----------------------------------------------------------------------------

func tailHandler(ws *websocket.Conn) {
	var t *tail.Tail

	for {
		var err error
		var m message
		// receive a message using the codec
		if err = websocket.JSON.Receive(ws, &m); err != nil {
			if err != io.EOF {
				lg.Println("info: read:", err)
			}
			break
		}

		if m.Channel == "?" {
			lg.Print("debug: Requested hostname")
			if cfg.Host != "" {
				m2 := message{Channel: "?", Message: cfg.Host}
				if err = websocket.JSON.Send(ws, m2); err != nil {
					lg.Println("info: Can't send host:", err)
					break
				}
			}
			continue
		}
		if m.Channel == "#" {
			lg.Print("debug: Requested ping")
			if err = websocket.JSON.Send(ws, m); err != nil {
				lg.Println("info: Can't send pong:", err)
				break
			}
			continue
		}

		logs, err := loadLogs()
		if err != nil {
			lg.Println("info: loadlogs:", err)
			break
		}

		if m.Channel == "" {
			lg.Print("debug: Requested channel list")
			m2 := listmessage{Message: logs}
			if err = websocket.JSON.Send(ws, m2); err != nil {
				lg.Println("info: Can't send:", err)
				break
			}
			continue
		}

		lg.Printf("debug: Requested channel %s", m.Channel)
		if _, ok := logs[m.Channel]; !ok {
			err = errors.New("Unknown logfile")
			m2 := message{Channel: m.Channel, Error: err.Error()}
			if err = websocket.JSON.Send(ws, m2); err != nil {
				lg.Println("info: Can't send error:", err)
			}
			break
		}

		config := tail.Config{
			Follow: true,
			ReOpen: true,
		}
		config.MaxLineSize = cfg.MaxLineSize
		config.Poll = cfg.Poll
		filename := path.Join(cfg.Root, m.Channel)
		lineIncomlete := false

		if cfg.Back != 0 {
			fi, err := os.Stat(filename)
			if err != nil {
				lg.Println("warn: file stat: ", err)
				break
			}
			// get the file size
			size := fi.Size()
			if size > cfg.Back {
				config.Location = &tail.SeekInfo{-cfg.Back, os.SEEK_END}
				lineIncomlete = true
			}
		}
		t, err = tail.TailFile(filename, config)
		if err != nil {
			lg.Println("warn: tail start: ", err)
			break
		}

		for line := range t.Lines {
			if lineIncomlete {
				// 1st line after offset might be incomplete - so skip it
				lineIncomlete = false
				continue
			}
			// send a response
			lg.Printf("debug: Sending line: %s", line.Text)
			m2 := message{Channel: m.Channel, Message: line.Text}
			if err = websocket.JSON.Send(ws, m2); err != nil {
				// TODO: print if not "write: broken pipe" error - lg.Println("info: Can't send:", err)
				break
			}
		}
		err = t.Wait()
		if err != nil {
			lg.Println("warn: tail run: ", err)
			break
		}
		lg.Println("info: Stop")
	}
	if t != nil {
		lg.Println("info: Cleanup")
		t.Cleanup()
	}
}

func main() {

	setUp(&cfg)
	lg.Printf("info: %s v %s. WebTail, tail logfiles via web", path.Base(os.Args[0]), Version)
	lg.Print("info: Copyright (C) 2016, Alexey Kovrizhkin <ak@elfire.ru>")

	_, err := os.Stat(cfg.Root)
	panicIfError(err, "Logfile root dir")

	logs, err := loadLogs()
	panicIfError(err, "Load logfile list")
	lg.Printf("info: Logfiles root %s contains %d item(s)", cfg.Root, len(logs))

	http.Handle("/tail", websocket.Handler(tailHandler))
	http.Handle("/", http.FileServer(assetFS()))

	lg.Printf("info: Listen at http://%s", cfg.Addr)
	err = http.ListenAndServe(cfg.Addr, nil)
	panicIfError(err, "Listen")
}

// -----------------------------------------------------------------------------

func setUp(cfg *Config) (err error) {

	p := flags.NewParser(cfg, flags.Default)

	_, err = p.Parse()
	if err != nil {
		os.Exit(1) // error message written already
	}
	if cfg.Version {
		// show version & exit
		fmt.Printf("%s\n%s\n%s", Version, Build, Commit)
		os.Exit(0)
	}

	// use all CPU cores for maximum performance
	runtime.GOMAXPROCS(runtime.NumCPU())

	lvl, err := colog.ParseLevel(cfg.LogLevel)
	panicIfError(err, "Parse loglevel")

	cl := colog.NewCoLog(os.Stderr, "", log.Lshortfile|log.Ldate|log.Ltime)
	cl.SetMinLevel(lvl)
	cl.SetDefaultLevel(lvl)
	lg = cl.NewLogger()

	return
}

// -----------------------------------------------------------------------------

func panicIfError(err error, msg string) {
	if err != nil {
		lg.Printf("error: %s error: %s", msg, err.Error())
		os.Exit(1)
	}
}
