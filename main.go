package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yinghuocho/autoupdate-server/args"
	"github.com/yinghuocho/golibfq/utils"
)

const (
	githubRefreshTime     = time.Minute * 10
	localPatchesDirectory = "./patches/"
)

var (
	flagPrivateKey         = flag.String("k", "./private.pem", "Path to private key.")
	flagLocalAddr          = flag.String("l", "127.0.0.1:6868", "Local bind address.")
	flagPublicAddr         = flag.String("p", "https://update.gofirefly.org/", "Public address.")
	flagGithubOrganization = flag.String("o", "yinghuocho", "Github organization.")
	flagGithubProject      = flag.String("n", "firefly-proxy", "Github project name.")
	flagAssetDir           = flag.String("asset", "./assets/", "asset directory.")
	flagPatchDir           = flag.String("patch", "./patches/", "patch directory.")
	flagPidFile            = flag.String("pid", ".", "pid file")
	flagLogFile            = flag.String("log", ".", "log file")
	flagHelp               = flag.Bool("h", false, "Shows help.")
)

var (
	releaseManager *ReleaseManager
)

type updateHandler struct{}

// updateAssets checks for new assets released on the github releases page.
func updateAssets() error {
	log.Printf("Updating assets...")
	if err := releaseManager.UpdateAssetsMap(); err != nil {
		return err
	}
	return nil
}

// backgroundUpdate periodically looks for releases.
func backgroundUpdate() {
	for {
		time.Sleep(githubRefreshTime)
		// Updating assets...
		if err := updateAssets(); err != nil {
			log.Printf("updateAssets: %s", err)
		}
	}
}

func (u *updateHandler) closeWithStatus(w http.ResponseWriter, status int) {
	w.WriteHeader(status)
	w.Write([]byte(http.StatusText(status)))
}

func (u *updateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var err error
	var res *args.Result

	if r.Method == "POST" {
		defer r.Body.Close()

		var params args.Params
		decoder := json.NewDecoder(r.Body)

		if err = decoder.Decode(&params); err != nil {
			u.closeWithStatus(w, http.StatusBadRequest)
			return
		}

		if res, err = releaseManager.CheckForUpdate(&params); err != nil {
			log.Printf("CheckForUpdate failed with error: %q", err)
			if err == ErrNoUpdateAvailable {
				u.closeWithStatus(w, http.StatusNoContent)
				return
			}
			u.closeWithStatus(w, http.StatusExpectationFailed)
			return
		}

		if res.PatchURL != "" {
			res.PatchURL = *flagPublicAddr + res.PatchURL
		}

		var content []byte

		if content, err = json.Marshal(res); err != nil {
			u.closeWithStatus(w, http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		w.Write(content)
		return
	}
	u.closeWithStatus(w, http.StatusNotFound)
	return
}

func loadPrivateKey(filename string) (*rsa.PrivateKey, error) {
	data, e := ioutil.ReadFile(filename)
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("couldn't decode PEM file")
	}
	privKey, e := x509.ParsePKCS1PrivateKey(block.Bytes)
	if e != nil {
		return nil, e
	}
	return privKey, nil
}

func main() {
	flag.Parse()
	if *flagHelp || *flagPrivateKey == "" {
		flag.Usage()
		os.Exit(0)
	}
	privKey, e := loadPrivateKey(*flagPrivateKey)
	if e != nil {
		log.Fatalf("fail to load private key: %s", e)
	}
	if !dirExists(*flagAssetDir) {
		e = os.MkdirAll(*flagAssetDir, 0755)
		if e != nil {
			log.Fatalf("fail to create asset dir: %s", e)
		}
	}
	if !dirExists(*flagPatchDir) {
		e = os.MkdirAll(*flagPatchDir, 0755)
		if e != nil {
			log.Fatalf("fail to create patch dir: %s", e)
		}
	}

	// initiate log file
	logFile := utils.RotateLog(*flagLogFile, nil)
	if *flagLogFile != "" && logFile == nil {
		log.Printf("WARNING: fail to initiate log file")
	}

	// pid file
	utils.SavePid(*flagPidFile)

	// Creating release manager.
	log.Printf("Starting release manager.")
	releaseManager = NewReleaseManager(*flagGithubOrganization, *flagGithubProject, *flagAssetDir, *flagPatchDir, privKey)
	updateAssets()

	// Setting a goroutine for pulling updates periodically
	go backgroundUpdate()

	mux := http.NewServeMux()
	mux.Handle("/update", new(updateHandler))
	mux.Handle("/patches/", http.StripPrefix("/patches/", http.FileServer(http.Dir(localPatchesDirectory))))

	srv := http.Server{
		Addr:    *flagLocalAddr,
		Handler: mux,
	}

	log.Printf("Starting up HTTP server at %s.", *flagLocalAddr)
	quit := make(chan bool)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("ListenAndServe: ", err)
			close(quit)
		}
	}()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	running := true
	for running == true {
		select {
		case s := <-ch:
			switch s {
			case syscall.SIGHUP:
				utils.RotateLog(*flagLogFile, logFile)
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT:
				log.Printf("Got signal \"%s\", exiting...", s)
				running = false
			}
		case <-quit:
			running = false
		}
	}
	log.Printf("done")
}
