/*
 * This file is part of Chihaya.
 *
 * Chihaya is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Chihaya is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with Chihaya.  If not, see <http://www.gnu.org/licenses/>.
 */

package server

import (
	"bytes"
	"chihaya/config"
	cdb "chihaya/database"
	"chihaya/record"
	"chihaya/util"
	"fmt"
	"github.com/zeebo/bencode"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type httpHandler struct {
	db         *cdb.Database
	bufferPool *util.BufferPool
	waitGroup  sync.WaitGroup
	startTime  time.Time
	terminate  bool

	// Internal stats
	deltaRequests int64
	throughput    int64
}

type queryParams struct {
	params     map[string]string
	infoHashes []string
}

func (p *queryParams) get(which string) (ret string, exists bool) {
	ret, exists = p.params[which]
	return
}

func (p *queryParams) getUint64(which string) (ret uint64, exists bool) {
	str, exists := p.params[which]
	if exists {
		var err error
		exists = false
		ret, err = strconv.ParseUint(str, 10, 64)
		if err == nil {
			exists = true
		}
	}
	return
}

func failure(err string, buf *bytes.Buffer, interval time.Duration) {
	failureData := make(map[string]interface{})
	failureData["failure reason"] = err
	failureData["interval"] = interval / time.Second     // Assuming in seconds
	failureData["min interval"] = interval / time.Second // Assuming in seconds
	data, errz := bencode.EncodeBytes(failureData)
	if errz != nil {
		panic(err)
	}
	buf.Write(data)
}

/*
 * URL.Query() is rather slow, so I rewrote it
 * Since the only parameter that can have multiple values is info_hash for scrapes, handle this specifically
 */
func (handler *httpHandler) parseQuery(query string) (ret *queryParams, err error) {
	ret = &queryParams{make(map[string]string), nil}
	queryLen := len(query)

	onKey := true

	var keyStart int
	var keyEnd int
	var valStart int
	var valEnd int

	hasInfoHash := false
	var firstInfoHash string

	for i := 0; i < queryLen; i++ {
		separator := query[i] == '&' || query[i] == ';'
		if separator || i == queryLen-1 { // ';' is a valid separator as per W3C spec
			if onKey {
				keyStart = i + 1
				continue
			}

			if i == queryLen-1 && !separator {
				if query[i] == '=' {
					continue
				}
				valEnd = i
			}

			keyStr, err1 := url.QueryUnescape(query[keyStart : keyEnd+1])
			if err1 != nil {
				err = err1
				return
			}
			valStr, err1 := url.QueryUnescape(query[valStart : valEnd+1])
			if err1 != nil {
				err = err1
				return
			}

			ret.params[keyStr] = valStr

			if keyStr == "info_hash" {
				if hasInfoHash {
					// There is more than one info_hash
					if ret.infoHashes == nil {
						ret.infoHashes = []string{firstInfoHash}
					}
					ret.infoHashes = append(ret.infoHashes, valStr)
				} else {
					firstInfoHash = valStr
					hasInfoHash = true
				}
			}
			onKey = true
			keyStart = i + 1
		} else if query[i] == '=' {
			onKey = false
			valStart = i + 1
		} else if onKey {
			keyEnd = i
		} else {
			valEnd = i
		}
	}
	return
}

func (handler *httpHandler) respond(r *http.Request, buf *bytes.Buffer) {
	dir, action := path.Split(r.URL.Path)
	if len(dir) != 34 {
		failure("Malformed request - missing passkey", buf, 1*time.Hour)
		return
	}

	passkey := dir[1:33]

	params, err := handler.parseQuery(r.URL.RawQuery)

	if err != nil {
		failure("Error parsing query", buf, 1*time.Hour)
		return
	}

	handler.db.UsersMutex.RLock()
	user, exists := handler.db.Users[passkey]
	handler.db.UsersMutex.RUnlock()
	if !exists {
		failure("Your passkey is invalid", buf, 1*time.Hour)
		return
	}

	ipAddr, exists := params.get("ipv4") // first try to get ipv4 address if client sent it
	if !exists {
		ipAddr, exists = params.get("ip")      // then try to get public ip if sent by client
		ipBytes := (net.ParseIP(ipAddr)).To4() // and make sure it is ipv4 one
		if !exists || nil == ipBytes {         // finally, if there is no ip sent by client in http request or ip sent is ipv6 only ...
			ips, exists := r.Header["X-Real-Ip"] // ... check if there is X-Real-Ip header sent by proxy?
			if exists && len(ips) > 0 {          // if yes, assume it
				ipAddr = ips[0]
			} else { // if not, assume ip to be in socket
				portIndex := len(r.RemoteAddr) - 1
				for ; portIndex >= 0; portIndex-- {
					if r.RemoteAddr[portIndex] == ':' {
						break
					}
				}
				if portIndex != -1 { // read ip from socket
					ipAddr = r.RemoteAddr[0:portIndex]
				} else { // if everything failed, abort request
					failure("Failed to parse IP address", buf, 1*time.Hour)
					return
				}
			}
		}
	}

	ipBytes := (net.ParseIP(ipAddr)).To4()
	if nil == ipBytes {
		failure("Assertion failed (net.ParseIP(ipAddr)).To4() == nil)! please report this issue to staff", buf, 1*time.Hour)
		return
	}

	switch action {
	case "announce":
		announce(params, user, ipAddr, handler.db, buf)
		return
	case "scrape":
		scrape(params, handler.db, buf)
		return
	}

	failure("Unknown action", buf, 1*time.Hour)
}

var handler *httpHandler
var listener net.Listener

func (handler *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if handler.terminate {
		return
	}
	handler.waitGroup.Add(1)
	defer handler.waitGroup.Done()

	defer func() {
		err := recover()
		if err != nil {
			log.Printf("!!! ServeHTTP panic !!! %v", err)
		}
	}()

	buf := handler.bufferPool.Take()
	defer handler.bufferPool.Give(buf)

	if r.URL.Path == "/stats" {
		db := handler.db
		peers := 0

		db.UsersMutex.RLock()
		db.TorrentsMutex.RLock()

		for _, t := range db.Torrents {
			peers += len(t.Leechers) + len(t.Seeders)
		}

		buf.WriteString(fmt.Sprintf("Uptime: %f\nUsers: %d\nTorrents: %d\nPeers: %d\nThroughput: %d rpm\n",
			time.Since(handler.startTime).Seconds(),
			len(db.Users),
			len(db.Torrents),
			peers,
			handler.throughput,
		))

		db.UsersMutex.RUnlock()
		db.TorrentsMutex.RUnlock()
	} else {
		handler.respond(r, buf)
	}

	w.Header().Add("Content-Type", "text/plain")
	w.Header().Add("Content-Length", strconv.Itoa(buf.Len()))

	// The response should always be 200, even on failure
	_, _ = w.Write(buf.Bytes())

	atomic.AddInt64(&handler.deltaRequests, 1)

	w.(http.Flusher).Flush()
}

func Start() {
	var err error

	handler = &httpHandler{db: &cdb.Database{}, startTime: time.Now()}

	bufferPool := util.NewBufferPool(500, 500)
	handler.bufferPool = bufferPool

	server := &http.Server{
		Handler:     handler,
		ReadTimeout: 20 * time.Second,
	}

	go collectStatistics()

	handler.db.Init()
	record.Init()

	listener, err = net.Listen("tcp", config.Get("addr"))

	if err != nil {
		panic(err)
	}

	/*
	 * Behind the scenes, this works by spawning a new goroutine for each client.
	 * This is pretty fast and scalable since goroutines are nice and efficient.
	 */
	_ = server.Serve(listener)

	// Wait for active connections to finish processing
	handler.waitGroup.Wait()

	handler.db.Terminate()

	log.Println("Shutdown complete")
}

func Stop() {
	// Closing the listener stops accepting connections and causes Serve to return
	_ = listener.Close()
	handler.terminate = true
}

func collectStatistics() {
	lastTime := time.Now()
	for {
		time.Sleep(time.Minute)
		duration := time.Since(lastTime)
		handler.throughput = int64(float64(handler.deltaRequests)/duration.Seconds()*60 + 0.5)
		atomic.StoreInt64(&handler.deltaRequests, 0)

		log.Printf("Throughput: %d rpm\n", handler.throughput)
		lastTime = time.Now()
	}
}
