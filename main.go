package main

import (
	"errors"
	"log"
	"os"

	"github.com/SaeedHurzuk/simple-torrent/server"
	"github.com/jpillora/opts"
)

var VERSION = "0.0.0-src" //set with ldflags

func main() {
	s := server.Server{
		Title:  "Private Torrent",
		Port:   3000, // depreciated
		Listen: ":4202",
	}

	o := opts.New(&s)
	o.Version(VERSION)
	o.Repo("https://github.com/SaeedHurzuk/simple-torrent")
	o.PkgRepo()
	o.SetLineWidth(96)
	o.Parse()

	if s.DisableLogTime {
		log.SetFlags(0)
	}

	log.Printf("############# PrivateTorrent ver[%s] #############\n", VERSION)
	if err := s.Run(VERSION); err != nil {
		if errors.Is(err, server.ErrDiskSpace) {
			log.Println(err)
			os.Exit(42)
		}
		log.Fatal(err)
	}
}
