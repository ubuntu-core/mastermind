package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var httpFlag = flag.String("http", ":8080", "Serve HTTP at given address")
var httpsFlag = flag.String("https", "", "Serve HTTPS at given address")
var certFlag = flag.String("cert", "", "Use the provided TLS certificate")
var keyFlag = flag.String("key", "", "Use the provided TLS key")

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()

	http.HandleFunc("/", handler)

	if *httpFlag == "" && *httpsFlag == "" {
		return fmt.Errorf("must provide -http and/or -https")
	}
	if (*httpsFlag != "" || *certFlag != "" || *keyFlag != "") && (*httpsFlag == "" || *certFlag == "" || *keyFlag == "") {
		return fmt.Errorf("-https -cert and -key must be used together")
	}

	ch := make(chan error, 2)

	if *httpFlag != "" {
		go func() {
			ch <- http.ListenAndServe(*httpFlag, nil)
		}()
	}
	if *httpsFlag != "" {
		go func() {
			ch <- http.ListenAndServeTLS(*httpsFlag, *certFlag, *keyFlag, nil)
		}()
	}
	return <-ch
}

// Repo represents a source code repository on GitHub.
type Repo struct {
	User    string
	Name    string
	Branch  string
	SubPath string
}

// GitHubRoot returns the repository root at GitHub, without a schema.
func (repo *Repo) GitHubRoot() string {
	return "github.com/" + repo.User + "/" + repo.Name
}

var pattern = regexp.MustCompile(`^/([a-zA-Z0-9][-a-zA-Z0-9]+)/([a-zA-Z][-.a-zA-Z0-9]*):([a-zA-Z0-9][-.a-zA-Z0-9]*)(?:\.git)?((?:/[a-zA-Z0-9][-.a-zA-Z0-9]*)*)$`)

func handler(resp http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/health-check" {
		resp.Write([]byte("ok"))
		return
	}

	log.Printf("%s requested %s", req.RemoteAddr, req.URL)

	if req.URL.Path == "/" {
		sendPlaceHolder(resp)
		return
	}

	m := pattern.FindStringSubmatch(req.URL.Path)
	if m == nil {
		sendNotFound(resp, "Unsupported URL pattern.")
		return
	}

	repo := &Repo{
		User:        m[1],
		Name:        m[2],
		Branch:      m[3],
		SubPath:     m[4],
	}

	var changed []byte
	original, err := fetchRefs(repo)
	if err == nil {
		changed, err = changeRefs(original, repo.Branch)
	}

	switch err {
	case nil:
		// all ok
	case ErrNoRepo:
		sendNotFound(resp, "GitHub repository not found at https://%s", repo.GitHubRoot())
		return
	case ErrNoBranch:
		sendNotFound(resp, `GitHub repository at https://%s has no branch or tag "%s"`, repo.GitHubRoot(), repo.Branch)
		return
	default:
		resp.WriteHeader(http.StatusBadGateway)
		resp.Write([]byte(fmt.Sprintf("Cannot obtain refs from GitHub: %v", err)))
		return
	}

	if repo.SubPath == "/git-upload-pack" {
		upresp, err := fetchUploadPack(repo, req)
		if err != nil {
			log.Printf("Cannot obtain upload pack from GitHub: %v", err)
			resp.WriteHeader(http.StatusBadGateway)
			resp.Write([]byte(fmt.Sprintf("Cannot obtain upload pack from GitHub: %v", err)))
			return
		}
		defer upresp.Body.Close()
		for name, _ := range upresp.Header {
			resp.Header().Set(name, upresp.Header.Get(name))
		}
		io.Copy(resp, upresp.Body)
		return
	}

	if repo.SubPath == "/info/refs" {
		resp.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		resp.Write(changed)
		return
	}

	sendPlaceHolder(resp)
}

func sendPlaceHolder(resp http.ResponseWriter) {
	resp.Header().Set("Content-Type", "text/plain")
	resp.Write([]byte("Use your creativity and build in your mind an elegant web page in this blank space."))
}

func sendNotFound(resp http.ResponseWriter, msg string, args ...interface{}) {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	resp.WriteHeader(http.StatusNotFound)
	resp.Write([]byte(msg))
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

const refsSuffix = ".git/info/refs?service=git-upload-pack"

var ErrNoRepo = errors.New("repository not found in GitHub")
var ErrNoBranch = errors.New("branch not found in GitHub")

func fetchRefs(repo *Repo) (data []byte, err error) {
	resp, err := httpClient.Get("https://" + repo.GitHubRoot() + refsSuffix)
	if err != nil {
		return nil, fmt.Errorf("cannot talk to GitHub: %v", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		// ok
	case 401, 404:
		return nil, ErrNoRepo
	default:
		return nil, fmt.Errorf("error from GitHub: %v", resp.Status)
	}

	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading from GitHub: %v", err)
	}
	return data, err
}

func fetchUploadPack(repo *Repo, req *http.Request) (resp *http.Response, err error) {
	upreq, err := http.NewRequest("POST", "https://"+repo.GitHubRoot()+"/git-upload-pack", req.Body)
	for name, value := range req.Header {
		upreq.Header[name] = value
	}
	upreq.Header["User-Agent"] = []string{"git/2.1.4"}
	resp, err = httpClient.Do(upreq)
	if err != nil {
		return nil, fmt.Errorf("cannot talk to GitHub: %v", err)
	}
	switch resp.StatusCode {
	case 200:
		// ok
	case 401, 404:
		resp.Body.Close()
		return nil, ErrNoRepo
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("error from GitHub: %v", resp.Status)
	}
	return resp, nil
}

func changeRefs(data []byte, branch string) (changed []byte, err error) {
	var hlinei, hlinej int // HEAD reference line start/end
	var mlinei, mlinej int // master reference line start/end
	var branchName string
	var branchHash string

	sdata := string(data)
	for i, j := 0, 0; i < len(data); i = j {
		size, err := strconv.ParseInt(sdata[i:i+4], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("cannot parse refs line size: %s", string(data[i:i+4]))
		}
		if size == 0 {
			size = 4
		}
		j = i + int(size)
		if j > len(sdata) {
			return nil, fmt.Errorf("incomplete refs data received from GitHub")
		}
		if sdata[0] == '#' {
			continue
		}

		hashi := i + 4
		hashj := strings.IndexByte(sdata[hashi:j], ' ')
		if hashj < 0 || hashj != 40 {
			continue
		}
		hashj += hashi

		namei := hashj + 1
		namej := strings.IndexAny(sdata[namei:j], "\n\x00")
		if namej < 0 {
			namej = j
		} else {
			namej += namei
		}

		name := sdata[namei:namej]

		if name == "HEAD" {
			hlinei = i
			hlinej = j
		}
		if name == "refs/heads/master" {
			mlinei = i
			mlinej = j
		}

		// Annotated tag is peeled off and overrides the same version just parsed.
		name = strings.TrimSuffix(name, "^{}")
		if name == "refs/heads/" + branch || name == "refs/tags/" + branch {
			branchHash = sdata[hashi:hashj]
			branchName = name
		}
	}

	// If the file has no HEAD line or the version was not found, report as unavailable.
	if hlinei == 0 || branchHash == "" {
		return nil, ErrNoBranch
	}

	var buf bytes.Buffer
	buf.Grow(len(data) + 256)

	// Copy the header as-is.
	buf.Write(data[:hlinei])

	// Extract the original capabilities.
	caps := ""
	if i := strings.Index(sdata[hlinei:hlinej], "\x00"); i > 0 {
		caps = strings.Replace(sdata[hlinei+i+1:hlinej-1], "symref=", "oldref=", -1)
	}

	// Insert the HEAD reference line with the right hash and a proper symref capability.
	var line string
	if strings.HasPrefix(branchName, "refs/heads/") {
		if caps == "" {
			line = fmt.Sprintf("%s HEAD\x00symref=HEAD:%s\n", branchHash, branchName)
		} else {
			line = fmt.Sprintf("%s HEAD\x00symref=HEAD:%s %s\n", branchHash, branchName, caps)
		}
	} else {
		if caps == "" {
			line = fmt.Sprintf("%s HEAD\n", branchHash)
		} else {
			line = fmt.Sprintf("%s HEAD\x00%s\n", branchHash, caps)
		}
	}
	fmt.Fprintf(&buf, "%04x%s", 4+len(line), line)

	// Insert the master reference line.
	line = fmt.Sprintf("%s refs/heads/master\n", branchHash)
	fmt.Fprintf(&buf, "%04x%s", 4+len(line), line)

	// Append the rest, dropping the original master line if necessary.
	if mlinei > 0 {
		buf.Write(data[hlinej:mlinei])
		buf.Write(data[mlinej:])
	} else {
		buf.Write(data[hlinej:])
	}

	return buf.Bytes(), nil
}
