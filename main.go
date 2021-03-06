// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/gcloud/gcs"
	"github.com/jacobsa/syncutil"
	"github.com/jgeewax/cli"
)

////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////

func registerSIGINTHandler(mountPoint string) {
	// Register for SIGINT.
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)

	// Start a goroutine that will unmount when the signal is received.
	go func() {
		for {
			<-signalChan
			log.Println("Received SIGINT, attempting to unmount...")

			err := fuse.Unmount(mountPoint)
			if err != nil {
				log.Printf("Failed to unmount in response to SIGINT: %v", err)
			} else {
				log.Printf("Successfully unmounted in response to SIGINT.")
				return
			}
		}
	}()
}

// Dump profiles on SIGHUP, if enabled.
func registerSIGHUPHandler(cpu bool, mem bool) {
	var desc string
	switch {
	case cpu && mem:
		desc = "CPU and memory profiles"

	case cpu:
		desc = "CPU profile"

	case mem:
		desc = "memory profile"

	default:
		return
	}

	const duration = 10 * time.Second
	profileOnce := func() (err error) {
		// CPU
		if cpu {
			var f *os.File
			f, err = os.Create("/tmp/cpu.pprof")
			if err != nil {
				err = fmt.Errorf("Create: %v", err)
				return
			}

			defer func() {
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
			}()

			pprof.StartCPUProfile(f)
			defer pprof.StopCPUProfile()
		}

		// Memory
		if mem {
			var f *os.File
			f, err = os.Create("/tmp/mem.pprof")
			if err != nil {
				err = fmt.Errorf("Create: %v", err)
				return
			}

			defer func() {
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
			}()

			defer func() {
				pprof.Lookup("heap").WriteTo(f, 0)
			}()
		}

		time.Sleep(duration)
		return
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)

	// Wait for SIGHUP in the background.
	go func() {
		for {
			<-c
			log.Printf("Received SIGHUP. Dumping %s to /tmp...", desc)
			if err := profileOnce(); err != nil {
				log.Printf("Error profiling: %v", err)
			} else {
				log.Println("Done profiling.")
			}
		}
	}()
}

// Create token source from the JSON file at the supplide path.
func newTokenSourceFromPath(
	path string,
	scope string) (ts oauth2.TokenSource, err error) {
	// Read the file.
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		err = fmt.Errorf("ReadFile(%q): %v", path, err)
		return
	}

	// Create a config struct based on its contents.
	jwtConfig, err := google.JWTConfigFromJSON(contents, scope)
	if err != nil {
		err = fmt.Errorf("JWTConfigFromJSON: %v", err)
		return
	}

	// Create the token source.
	ts = jwtConfig.TokenSource(context.Background())

	return
}

func getConn(flags *flagStorage) (c gcs.Conn, err error) {
	// Create the oauth2 token source.
	const scope = gcs.Scope_FullControl

	var tokenSrc oauth2.TokenSource
	if flags.KeyFile != "" {
		tokenSrc, err = newTokenSourceFromPath(flags.KeyFile, scope)
		if err != nil {
			err = fmt.Errorf("newTokenSourceFromPath: %v", err)
			return
		}
	} else {
		tokenSrc, err = google.DefaultTokenSource(context.Background(), scope)
		if err != nil {
			err = fmt.Errorf("DefaultTokenSource: %v", err)
			return
		}
	}

	// Create the connection.
	const userAgent = "gcsfuse/0.0"
	cfg := &gcs.ConnConfig{
		TokenSource: tokenSrc,
		UserAgent:   userAgent,
	}

	if flags.DebugHTTP {
		cfg.HTTPDebugLogger = log.New(os.Stderr, "http: ", 0)
	}

	if flags.DebugGCS {
		cfg.GCSDebugLogger = log.New(os.Stderr, "gcs: ", 0)
	}

	return gcs.NewConn(cfg)
}

////////////////////////////////////////////////////////////////////////
// main function
////////////////////////////////////////////////////////////////////////

func main() {
	// Make logging output better.
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	app := newApp()
	app.Action = func(c *cli.Context) {
		var err error

		// We should get two arguments exactly. Otherwise error out.
		if len(c.Args()) != 2 {
			fmt.Fprintf(
				os.Stderr,
				"Error: %s takes exactly two arguments.\n\n",
				app.Name)
			cli.ShowAppHelp(c)
			os.Exit(1)
		}

		// Populate and parse flags.
		bucketName := c.Args()[0]
		mountPoint := c.Args()[1]
		flags := populateFlags(c)

		// Enable invariant checking if requested.
		if flags.DebugInvariants {
			syncutil.EnableInvariantChecking()
		}

		// Enable profiling if requested.
		registerSIGHUPHandler(flags.DebugCPUProfile, flags.DebugMemProfile)

		// Grab the connection.
		conn, err := getConn(flags)
		if err != nil {
			log.Fatalf("getConn: %v", err)
		}

		// Mount the file system.
		mfs, err := mount(
			context.Background(),
			bucketName,
			mountPoint,
			flags,
			conn)

		if err != nil {
			log.Fatalf("Mounting file system: %v", err)
		}

		log.Println("File system has been successfully mounted.")

		// Let the user unmount with Ctrl-C (SIGINT).
		registerSIGINTHandler(mfs.Dir())

		// Wait for the file system to be unmounted.
		err = mfs.Join(context.Background())
		if err != nil {
			err = fmt.Errorf("MountedFileSystem.Join: %v", err)
			return
		}

		log.Println("Successfully exiting.")
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatalln(err)
	}
}
