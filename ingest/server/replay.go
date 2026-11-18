package server

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mucan54/izlek/ingest/wal"
	"github.com/mucan54/izlek/ingest/wire"
)

const (
	replayMaxBody   = wire.MaxBatchBytes // spec/replay.md §4: body <= 1 MB
	replayMaxSeq    = 999_999            // key carries <seq:06>
	replayMaxIDLen  = 200                // anon_id / user_id limit of spec/wire.md §3
	replayEncZstd   = 1
	replayEncGzip   = 2
	replayPutBudget = 30 * time.Second
)

var errBadMagic = errors.New("body does not match its Content-Encoding")

// replayBody passes the request body through to the object store while
// enforcing the size limit and checking that the first bytes are a gzip or
// zstd frame, without buffering the chunk.
type replayBody struct {
	r     io.Reader
	magic []byte
	head  [4]byte
	n     int64
}

func (b *replayBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	for i := 0; i < n && b.n+int64(i) < int64(len(b.magic)); i++ {
		b.head[b.n+int64(i)] = p[i]
	}
	b.n += int64(n)
	// A violation is reported WITHOUT handing out the bytes of this read:
	// the object store must never receive a complete body that we reject.
	if b.n > replayMaxBody {
		return 0, errTooLarge
	}
	have := min(b.n, int64(len(b.magic)))
	if string(b.head[:have]) != string(b.magic[:have]) {
		return 0, errBadMagic
	}
	if err == io.EOF && b.n < int64(len(b.magic)) {
		return 0, errBadMagic // ended before a complete magic
	}
	return n, err
}

// handleReplay is POST /v1/r (spec/replay.md §4): the compressed chunk is
// streamed to object storage as received, then a format=3 index entry is
// appended to the WAL; 200 is sent after its fsync.
func (s *Server) handleReplay(r *http.Request, rc *reqCtx) reply {
	if r.Method != http.MethodPost {
		return reply{status: http.StatusMethodNotAllowed}
	}
	tok, rep, ok := s.authenticate(bearerToken(r), rc)
	if !ok {
		return rep
	}
	if !tok.Replay {
		return reply{status: http.StatusForbidden}
	}
	ip := s.clientIP(r)
	if s.tokenLimited(tok, rc.recv) || s.ipLimited(ip, rc.recv) {
		return reply{status: http.StatusTooManyRequests, retryAfter: retryRateLimited}
	}
	if s.overloaded() {
		return reply{status: http.StatusServiceUnavailable, retryAfter: retryBackpressure}
	}

	q := r.URL.Query()
	var sid [16]byte
	sess := strings.ToLower(q.Get("s"))
	if n, err := hex.Decode(sid[:], []byte(sess)); len(sess) != 32 || err != nil || n != 16 {
		return reply{status: http.StatusBadRequest}
	}
	seq, err1 := strconv.ParseUint(q.Get("n"), 10, 64)
	startTS, err2 := strconv.ParseUint(q.Get("ts"), 10, 64)
	duration, err3 := strconv.ParseUint(q.Get("d"), 10, 64)
	anon, user := q.Get("a"), q.Get("u")
	if err1 != nil || err2 != nil || err3 != nil || seq > replayMaxSeq ||
		anon == "" || len(anon) > replayMaxIDLen || len(user) > replayMaxIDLen {
		return reply{status: http.StatusBadRequest}
	}

	var ext string
	var encoding byte
	var magic []byte
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "zstd":
		ext, encoding, magic = "zst", replayEncZstd, []byte{0x28, 0xb5, 0x2f, 0xfd}
	case "gzip":
		ext, encoding, magic = "gz", replayEncGzip, []byte{0x1f, 0x8b}
	default:
		return reply{status: http.StatusBadRequest} // encoding is mandatory
	}
	if r.ContentLength > replayMaxBody {
		return reply{status: http.StatusRequestEntityTooLarge}
	}
	if r.ContentLength >= 0 && r.ContentLength < int64(len(magic)) {
		return reply{status: http.StatusBadRequest} // cannot even hold the frame magic
	}

	key := fmt.Sprintf("replay/p=%d/d=%s/%s/%06d.bin.%s", tok.ProjectID, rc.recv.UTC().Format("2006-01-02"), sess, seq, ext)
	body := &replayBody{r: r.Body, magic: magic}
	ctx, cancel := context.WithTimeout(r.Context(), replayPutBudget)
	_, err := s.store.PutStream(ctx, key, body, r.ContentLength)
	cancel()
	if err != nil {
		switch {
		case errors.Is(err, errTooLarge):
			return reply{status: http.StatusRequestEntityTooLarge}
		case errors.Is(err, errBadMagic):
			return reply{status: http.StatusBadRequest}
		case r.Context().Err() != nil:
			return reply{status: http.StatusBadRequest} // client went away mid-upload
		}
		s.log.Warn("replay upload failed", "request_id", rc.reqID, "err", err)
		return reply{status: http.StatusServiceUnavailable, retryAfter: retryTransient}
	}

	// format=3 index entry (spec/wal.md §2.3).
	ep := getBuf()
	defer putBuf(ep)
	entry := append((*ep)[:0], make([]byte, wal.EntryPrefixLen)...)
	entry = append(entry, sid[:]...)
	entry = binary.AppendUvarint(entry, seq)
	entry = binary.AppendUvarint(entry, startTS)
	entry = binary.AppendUvarint(entry, duration)
	entry = binary.AppendUvarint(entry, uint64(body.n))
	entry = append(entry, encoding)
	for _, str := range [...]string{anon, user, key} {
		entry = binary.AppendUvarint(entry, uint64(len(str)))
		entry = append(entry, str...)
	}
	*ep = entry
	if err := wal.FinishEntry(entry, wal.Envelope{
		Format:    wal.FormatReplay,
		ProjectID: tok.ProjectID,
		RecvTSMs:  rc.recvMs(),
		Country:   s.country(ip),
		Flags:     envFlags(rc),
	}); err != nil {
		return reply{status: http.StatusBadRequest}
	}
	if err := s.writer.Append(entry); err != nil {
		s.m.walWriteErrors.Inc()
		return reply{status: http.StatusServiceUnavailable, retryAfter: retryTransient}
	}
	s.m.bytesIn.Add(uint64(body.n))
	s.m.recordsIn.Inc()
	return reply{status: http.StatusOK}
}
