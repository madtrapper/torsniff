package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/marksamman/bencode"
	"github.com/spf13/cobra"
)

const (
	directory = "torrents"
)

func homeDir() string {
	env := "HOME"
	if runtime.GOOS == "windows" {
		env = "USERPROFILE"
	} else if runtime.GOOS == "plan9" {
		env = "home"
	}
	return os.Getenv(env)
}

type tfile struct {
	name   string
	length int64
}

func (t *tfile) String() string {
	return fmt.Sprintf("name: %s\n, size: %d\n", t.name, t.length)
}

type torrent struct {
	infohashHex string
	name        string
	length      int64
	files       []*tfile
}

func (t *torrent) String() string {
	return fmt.Sprintf(
		"link: %s\nname: %s\nsize: %d\nfile: %d\n",
		fmt.Sprintf("magnet:?xt=urn:btih:%s", t.infohashHex),
		t.name,
		t.length,
		len(t.files),
	)
}

func parseTorrent(meta []byte, infohashHex string) (*torrent, error) {
	dict, err := bencode.Decode(bytes.NewBuffer(meta))
	if err != nil {
		return nil, err
	}

	t := &torrent{infohashHex: infohashHex}
	if name, ok := dict["name.utf-8"].(string); ok {
		t.name = name
	} else if name, ok := dict["name"].(string); ok {
		t.name = name
	}
	if length, ok := dict["length"].(int64); ok {
		t.length = length
	}

	var totalSize int64
	var extractFiles = func(file map[string]interface{}) {
		var filename string
		var filelength int64
		if inter, ok := file["path.utf-8"].([]interface{}); ok {
			name := make([]string, len(inter))
			for i, v := range inter {
				name[i] = fmt.Sprint(v)
			}
			filename = strings.Join(name, "/")
		} else if inter, ok := file["path"].([]interface{}); ok {
			name := make([]string, len(inter))
			for i, v := range inter {
				name[i] = fmt.Sprint(v)
			}
			filename = strings.Join(name, "/")
		}
		if length, ok := file["length"].(int64); ok {
			filelength = length
			totalSize += filelength
		}
		t.files = append(t.files, &tfile{name: filename, length: filelength})
	}

	if files, ok := dict["files"].([]interface{}); ok {
		for _, file := range files {
			if f, ok := file.(map[string]interface{}); ok {
				extractFiles(f)
			}
		}
	}

	if t.length == 0 {
		t.length = totalSize
	}
	if len(t.files) == 0 {
		t.files = append(t.files, &tfile{name: t.name, length: t.length})
	}

	return t, nil
}

type torsniff struct {
	laddr      string
	maxFriends int
	maxPeers   int
	secret     string
	timeout    time.Duration
	blacklist  *blackList
	dir        string
}

func (t *torsniff) run() error {
	tokens := make(chan struct{}, t.maxPeers)

	dht, err := newDHT(t.laddr, t.maxFriends)
	if err != nil {
		return err
	}

	dht.run()

	log.Println("running, it may take a few minutes...")

	for {
		select {
		case <-dht.announcements.wait():
			for {
				if ac := dht.announcements.get(); ac != nil {
					tokens <- struct{}{}
					go t.work(ac, tokens)
					continue
				}
				break
			}
		case <-dht.die:
			return dht.errDie
		}
	}

	return nil
}

func (t *torsniff) work(ac *announcement, tokens chan struct{}) {
	defer func() {
		<-tokens
	}()

	if t.isTorrentExist(ac.infohashHex) {
		return
	}

	peerAddr := ac.peer.String()
	if t.blacklist.has(peerAddr) {
		return
	}

	wire := newMetaWire(string(ac.infohash), peerAddr, t.timeout)
	defer wire.free()

	meta, err := wire.fetch()
	if err != nil {
		t.blacklist.add(peerAddr)
		return
	}

	_, err = t.saveTorrent(ac.infohashHex, meta)
	if err != nil {
		return
	}

	torrent, err := parseTorrent(meta, ac.infohashHex)
	if err != nil {
		return
	}

	log.Println(torrent)
}

func (t *torsniff) isTorrentExist(infohashHex string) bool {
	name, _ := t.torrentPath(infohashHex)
	_, err := os.Stat(name)
	if os.IsNotExist(err) {
		return false
	}
	return err == nil
}

func (t *torsniff) saveTorrent(infohashHex string, data []byte) (string, error) {
	name, dir := t.torrentPath(infohashHex)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE, 0755)
	if err != nil {
		return "", err
	}

	defer f.Close()

	d, err := bencode.Decode(bytes.NewBuffer(data))
	if err != nil {
		return "", err
	}

	_, err = f.Write(bencode.Encode(map[string]interface{}{
		"info": d,
	}))
	if err != nil {
		return "", err
	}

	return name, nil
}

func (t *torsniff) torrentPath(infohashHex string) (name string, dir string) {
	dir = path.Join(t.dir, infohashHex[:2], infohashHex[len(infohashHex)-2:])
	name = path.Join(dir, infohashHex+".torrent")
	return
}

func main() {
	log.SetFlags(0)

	var addr string
	var port uint16
	var peers int
	var timeout time.Duration
	var dir string
	var verbose bool
	var friends int

	root := &cobra.Command{
		Use:          "torsniff",
		Short:        "torsniff - a sniffer fetching torrents from BitTorrent network",
		SilenceUsage: true,
	}
	root.RunE = func(cmd *cobra.Command, args []string) error {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return err
		}

		log.SetOutput(ioutil.Discard)
		if verbose {
			log.SetOutput(os.Stdout)
		}

		p := &torsniff{
			laddr:      fmt.Sprintf("%s:%d", addr, port),
			timeout:    timeout,
			maxFriends: friends,
			maxPeers:   peers,
			secret:     string(randBytes(20)),
			dir:        absDir,
			blacklist:  newBlackList(5*time.Minute, 50000),
		}
		return p.run()
	}
	root.Flags().StringVarP(&addr, "addr", "a", "0.0.0.0", "listen on given address")
	root.Flags().Uint16VarP(&port, "port", "p", 6881, "listen on given port")
	root.Flags().IntVarP(&friends, "friends", "f", 500, "max fiends to make with per second")
	root.Flags().IntVarP(&peers, "peers", "e", 400, "max peers to connect to download torrents")
	root.Flags().DurationVarP(&timeout, "timeout", "t", 10*time.Second, "max time allowed for downloading torrents")
	root.Flags().StringVarP(&dir, "dir", "d", path.Join(homeDir(), directory), "the directory to store the torrents")
	root.Flags().BoolVarP(&verbose, "verbose", "v", true, "run in verbose mode")

	if err := root.Execute(); err != nil {
		fmt.Errorf("could not start: %s\n", err)
	}
}
