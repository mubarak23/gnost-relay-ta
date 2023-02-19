package main

import (
	"bytes"
	"compress/flate"
	"context"
	crypto "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/gobwas/ws"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip26"
	"github.com/nbd-wtf/go-nostr/nip42"
)

type EventSubmission struct {
	event       *nostr.Event
	return_pool *sync.Pool // for returning the event pointer
	ctx         context.Context
	writer      io.WriteCloser
	cancel      context.CancelFunc
}
type ReqSubmission struct {
	addr    string
	id      string
	filters []ParsedFilter
	ctx     context.Context
	writer  io.WriteCloser
	query   *Query
	cancel  context.CancelFunc
}
type CloseSubmission struct {
	addr string
	id   string
	ctx  context.Context
}

func main() {
	// initilize db
	dbpool, err := InitStorage()
	if err != nil {
		panic(err)
	}

	// initialize the pools
	var any_buf_pool = sync.Pool{New: func() any { return make([]any, 0) }}
	var bytes_buf_pool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
	var event_pool = sync.Pool{New: func() any { return new(nostr.Event) }}
	var mask_buf_pool = sync.Pool{New: func() any { return make([]byte, read_buffer_size) }}
	var msg_pool = sync.Pool{New: func() any { return make([]json.RawMessage, 0) }}
	var string_buf_pool = sync.Pool{New: func() any { return make([]string, 0) }}

	dummy := bytes.NewBuffer(nil)
	var flate_writer_pool = sync.Pool{New: func() any {
		wr, err := flate.NewWriter(dummy, flate.BestSpeed)
		if err != nil {
			panic(err)
		}
		return wr
	}}
	var flate_reader_pool = sync.Pool{New: func() any {
		return flate.NewReader(dummy)
	}}

	// NIP_11_bytes.
	// Readonly so don't need Mutex
	nip_11_bytes, gzip_nip_11_bytes, err := NIP11_gzip_bytes()
	if err != nil {
		panic(err)
	}

	// for generating NIP-42 AUTH challenges
	// rand_reader is not safe for concurrent use
	var challenge_bytes [16]byte
	crypto.Reader.Read(challenge_bytes[:])
	var rand_mu sync.Mutex
	rand_reader := rand.New(rand.NewSource(time.Now().UnixNano() + int64(challenge_bytes[0]) + 256*int64(challenge_bytes[1]) + 256*256*int64(challenge_bytes[2]) + 256*256*256*int64(challenge_bytes[3]) + 256*256*256*256*int64(challenge_bytes[4])))

	// start the listener
	ln, err := net.Listen("tcp", "localhost:8080")
	if err != nil {
		panic(err)
	}

	event_chan := make(chan EventSubmission, 64)
	req_chan := make(chan ReqSubmission, 64)
	close_chan := make(chan CloseSubmission, 64)

	logger := log.Default()

	// start handlers
	go EventSubmissionHandler(event_chan, dbpool, logger)
	go ReqSubmissionHandler(req_chan, close_chan, dbpool, logger)

	upgrader := WS_upgrader()

	for {
		conn, err := ln.Accept()
		if err != nil {
			panic(err)
		}

		// one go routine per conn
		go func() {
			defer conn.Close()
			// pre-upgrade handler
			var handshake *ws.Handshake
			for {
				if hs, err := upgrader.Upgrade(conn); err != nil {
					// error in upgrade. check if hijacked.
					if e, ok := err.(*nip11_escape); ok == true {
						switch e.encoding {
						case [4]byte{'g', 'z', 'i', 'p'}:
							conn.Write(gzip_nip_11_bytes)
						default:
							conn.Write(nip_11_bytes)
						}
						// listen for more GET requests on this connection.
						continue
					} else {
						// error in upgrade, and was not hijacked by NIP11, so close and continue
						return
					}
				} else {
					// Upgrade successful! Break to main websocket handler.
					handshake = &hs
					break
				}
			}

			// allocate stuff for websocket connection
			ctx, cancel := context.WithCancel(context.Background())
			limiter := rate.NewLimiter(websocket_rate_limit, websocket_burst)
			defer cancel()

			// generate websocket_id
			rand_mu.Lock()
			rand_reader.Read(challenge_bytes[:])
			websocket_id := base64.StdEncoding.EncodeToString(challenge_bytes[:])
			rand_mu.Unlock()
			var authenticated_user string

			msgs, writer := handle_websocket(handshake, &bytes_buf_pool, &flate_reader_pool, &flate_writer_pool, &mask_buf_pool, &msg_pool, conn, logger)
			defer writer.Close()

			// send NIP-42 challenge
			if _, err := writer.Write([]byte(fmt.Sprintf("[\"AUTH\",\"%s\"]", websocket_id))); err != nil {
				return
			} else {
				writer.Write(flush_bytes[:])
			}

			// post upgrade handler
			for {
				// AUTH, EVENT, REQ, CLOSE
				msg := <-msgs
				if len(msg.jmsg) < 2 {
					msg.Release()
					if e := limiter.WaitN(ctx, websocket_burst); e != nil {
						logger.Println("connection closed", conn.RemoteAddr())
						return
					}
					continue
				}
				switch {
				case msg.jmsg[0][1] == 'A': // AUTH
					ev := event_pool.Get().(*nostr.Event)
					if err := json.Unmarshal(msg.jmsg[1], ev); err != nil {
						event_pool.Put(ev)
						if _, err := writer.Write([]byte("[\"NOTICE\",\"Invalid AUTH\"]")); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						// break hits the rate limiter
						break
					}
					if pub, ok := nip42.ValidateAuthEvent(ev, websocket_id, relay_url); ok == true {
						event_pool.Put(ev)
						authenticated_user = pub
						if _, err := writer.Write([]byte(fmt.Sprintf("[\"NOTICE\",\"Authenticated as %s\"]", pub))); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						msg.Release()
						continue
					} else {
						event_pool.Put(ev)
						if _, err := writer.Write([]byte("[\"NOTICE\",\"AUTH failed\"]")); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						// End of block acts as break. Will hit the rate limiter
					}
				case msg.jmsg[0][1] == 'E': // EVENT
					ev := event_pool.Get().(*nostr.Event)
					if err := json.Unmarshal(msg.jmsg[1], &ev); err != nil {
						event_pool.Put(ev)
						if _, err := writer.Write([]byte("[\"NOTICE\",\"Invalid EVENT\"]")); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						break
					}
					if b, e := ev.CheckSignature(); e != nil || b != true {
						event_pool.Put(ev)
						if _, err := writer.Write([]byte(fmt.Sprintf("[\"OK\",\"%s\",false,\"\"]", ev.ID))); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						break
					}
					event_chan <- EventSubmission{
						event:       ev,
						return_pool: &event_pool,
						ctx:         ctx,
						writer:      writer,
						cancel:      cancel,
					}
				case msg.jmsg[0][1] == 'R': // REQ
					filters := make([]ParsedFilter, 0)
					var id string
					if len(msg.jmsg) < 3 {
						if _, err := writer.Write([]byte("[\"NOTICE\",\"REQ too short\"]")); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						break
					}
					if err := json.Unmarshal(msg.jmsg[1], &id); err != nil {
						if _, err := writer.Write([]byte("[\"NOTICE\",\"Cannot parse REQ\"]")); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						break
					}
					if len(id) > subid_max_length {
						if _, err := writer.Write([]byte(fmt.Sprintf("[\"NOTICE\",\"subid is too long. max subid length is %d\"]", subid_max_length))); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						break
					}
					// parse all the filters and discard any invalid ones
					for _, f := range msg.jmsg[2:] {
						var filter ParsedFilter
						if err := json.Unmarshal(
							f,
							&filter,
						); err != nil {
							if _, e := writer.Write([]byte(fmt.Sprintf("[\"NOTICE\",\"Invalid filter in %s: %s\"]", id, err.Error()))); e != nil {
								return
							} else {
								writer.Write(flush_bytes[:])
							}
							goto skip
						}
						if len(filter.Kinds) == 0 {
							filter.Kinds = []int{1}
							// we know there is no kind 4
							// jump straight to append
							goto append
						}
						for _, k := range filter.Kinds {
							if k == 4 {
								switch {
								case authenticated_user != "" && len(filter.Authors) == 1 && authenticated_user == filter.Authors[0]:
									// sole author is authenticated user
									goto append
								case authenticated_user != "" && len(filter.Ptags) == 1 && authenticated_user == filter.Ptags[0]:
									// sole receiver is authenticated user
									goto append
								default:
									if _, e := writer.Write([]byte(fmt.Sprintf("[\"NOTICE\",\"Invalid filter in %s: user is not authenticated as sender or receiver.\"]", id))); e != nil {
										return
									} else {
										writer.Write(flush_bytes[:])
									}
									goto skip
								}
							}
						}
					append:
						filters = append(filters, filter)
					skip:
					}
					if len(filters) == 0 {
						if _, err := writer.Write([]byte("[\"NOTICE\",\"No filters were accepted. REQ Cancelled.\"]")); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						break
					}
					// generate the query
					// use buffer pools to optimize memory allocations
					query, err := SQL(filters, &string_buf_pool, &any_buf_pool)
					if err != nil {
						if _, e := writer.Write([]byte(fmt.Sprintf("[\"NOTICE\",\"SQL Query Error: %s\"]", err.Error()))); e != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						break
					}
					req_chan <- ReqSubmission{
						addr:    conn.RemoteAddr().String(),
						id:      id,
						filters: filters,
						ctx:     ctx,
						writer:  writer,
						cancel:  cancel,
						query:   query,
					}
				case msg.jmsg[0][1] == 'C': // CLOSE
					var id string
					var invalid bool
					if len(msg.jmsg) < 2 {
						invalid = true
					} else {
						err := json.Unmarshal(msg.jmsg[1], &id)
						invalid = err != nil
					}
					if invalid || len(id) == 0 || len(id) > subid_max_length {
						if _, err := writer.Write([]byte("[\"NOTICE\",\"Invalid CLOSE message\"]")); err != nil {
							return
						} else {
							writer.Write(flush_bytes[:])
						}
						break
					}
					close_chan <- CloseSubmission{
						addr: conn.RemoteAddr().String(),
						id:   id,
						ctx:  ctx,
					}
				}
				msg.Release()
				if e := limiter.Wait(ctx); e != nil {
					logger.Println("connection closed", conn.RemoteAddr())
					return
				}
			}
		}()
	}
}

func EventSubmissionHandler(event_chan chan EventSubmission, dbpool *pgxpool.Pool, logger *log.Logger) {
	limiter := rate.NewLimiter(25, 5)
	dbconn, e := dbpool.Acquire(context.Background())
	if e != nil {
		panic(e)
	}
	if t, e := dbconn.Exec(context.Background(), "DELETE FROM db1 WHERE expiration is not null and expiration < $1", time.Now().Unix()); e != nil {
		panic(e)
	} else {
		logger.Printf("database initialized: %d expired events deleted", t.RowsAffected())
	}

	ticker := time.NewTicker(delete_expired_events_period)
	delegation_token := new(nip26.DelegationToken)
	jsonbuf := bytes.NewBuffer(nil)
	ptags := make([]string, 0)
	etags := make([]string, 0)
	gtags := make([]string, 0)

	for {
		select {
		case <-ticker.C:
			if t, e := dbconn.Exec(context.Background(), "DELETE FROM db1 WHERE expiration is not null and expiration < $1", time.Now().Unix()); e != nil {
				panic(e)
			} else {
				logger.Printf("database cleanup: %d expired events deleted", t.RowsAffected())
			}
		case ev := <-event_chan:
			if e := limiter.Wait(ev.ctx); e == nil {
				if e := ev.store_event(dbconn, ptags, etags, gtags, delegation_token, jsonbuf); e != nil {
					if _, e := ev.writer.Write([]byte(fmt.Sprintf("[\"OK\",\"%s\",false,\"event not accepted into database\"]", ev.event.ID))); e != nil {
						ev.writer.Close()
						ev.cancel()
					} else {
						ev.writer.Write(flush_bytes[:])
					}
				} else {
					if _, e := ev.writer.Write([]byte(fmt.Sprintf("[\"OK\",\"%s\",true,\"\"]", ev.event.ID))); e != nil {
						ev.writer.Close()
						ev.cancel()
					} else {
						ev.writer.Write(flush_bytes[:])
					}
				}
			}
		}
	}

}

func ReqSubmissionHandler(req_chan chan ReqSubmission, close_chan chan CloseSubmission, dbpool *pgxpool.Pool, logger *log.Logger) {
	// mutex for the subids map
	var mu sync.Mutex
	subids := make(map[string]ReqSubmission)

	// handler for events newly added to DB.
	go func() {
		var new_conn *pgxpool.Conn
		if c, err := dbpool.Acquire(context.Background()); err == nil {
			new_conn = c
		} else {
			panic(err)
		}
		newsub := new(DBNotification)
		buf := bytes.NewBuffer(nil)
		if _, err := new_conn.Exec(context.Background(), "listen submissions"); err != nil {
			return
		}
		for {
			if notification, err := new_conn.Conn().WaitForNotification(context.Background()); err == nil {
				if err = json.Unmarshal([]byte(notification.Payload), newsub); err != nil {
					panic(err)
				}
			} else {
				panic(err)
			}
			mu.Lock()
			for sid, req := range subids {
				if req.ctx.Err() != nil {
					delete(subids, sid)
					continue
				}
				for _, f := range req.filters {
					if f.Accept(newsub) {
						buf.Reset()
						buf.Write([]byte(fmt.Sprintf("[\"EVENT\",\"%s\",", req.id)))
						buf.Write(newsub.Raw)
						buf.WriteByte(']')
						if _, e := req.writer.Write(buf.Bytes()); e != nil {
							req.writer.Close()
							req.cancel()
						} else {
							req.writer.Write(flush_bytes[:])
						}
						break
					}
				}
			}
			mu.Unlock()
		}
	}()

	// handler for incoming reqs.
	buf := bytes.NewBuffer(nil)
	pf_buf := make([]ParsedFilter, 0)
	raw := new(json.RawMessage)
	for {
		select {
		case req := <-req_chan:
			dbconn, e := dbpool.Acquire(req.ctx)
			if e != nil {
				select {
				case <-req.ctx.Done():
					continue
				default:
					panic(e)
				}
			}
			func() {
				defer dbconn.Release()
				defer req.query.Release()
				rows, e := dbconn.Query(req.ctx, req.query.sql, req.query.params...)
				if e != nil {
					select {
					case <-req.ctx.Done():
						return
					default:
						panic(e)
					}
				}
				for rows.Next() {
					if req.ctx.Err() != nil {
						return
					}
					buf.Reset()
					buf.Write([]byte(fmt.Sprintf("[\"EVENT\",\"%s\",", req.id)))
					if err := rows.Scan(raw); err != nil || raw == nil {
						panic(err)
					}
					buf.Write(*raw)
					buf.WriteByte(']')
					if _, e := req.writer.Write(buf.Bytes()); e != nil {
						req.writer.Close()
						req.cancel()
						return
					} else {
						req.writer.Write(flush_bytes[:])
					}
				}
				buf.Reset()
				buf.Write([]byte(fmt.Sprintf("[\"EOSE\",\"%s\"]", req.id)))
				if _, err := req.writer.Write(buf.Bytes()); err != nil {
					req.writer.Close()
					req.cancel()
					return
				} else {
					req.writer.Write(flush_bytes[:])
				}
				uid := req.addr + "/" + req.id
				if err := req.Cull(pf_buf); err != nil {
					return
				}
				mu.Lock()
				subids[uid] = req
				mu.Unlock()
			}()
		case close := <-close_chan:
			if len(close.id) > subid_max_length {
				continue
			}
			uid := close.addr + "/" + close.id
			mu.Lock()
			delete(subids, uid)
			mu.Unlock()
		}
	}
}
