package proxy

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/mcuadros/exmongodb/protocol"

	"gopkg.in/mgo.v2/bson"
)

type testWriter struct {
	write func([]byte) (int, error)
}

func (t testWriter) Write(b []byte) (int, error) { return t.write(b) }

// GetLastErrorRewriter handles getLastError requests and proxies, caches or
// sends cached responses as necessary.
type GetLastErrorRewriter struct {
	Log Logger `inject:""`
}

// Rewrite handles getLastError requests.
func (r *GetLastErrorRewriter) Rewrite(
	h *protocol.MessageHeader,
	parts [][]byte,
	client io.ReadWriter,
	server io.ReadWriter,
	lastError *protocol.LastError,
) error {

	if !lastError.Exists() {
		// We're going to be performing a real getLastError query and caching the
		// response.
		var written int
		for _, b := range parts {
			n, err := server.Write(b)
			if err != nil {
				r.Log.Error(err)
				return err
			}
			written += n
		}

		pending := int64(h.MessageLength) - int64(written)
		if _, err := io.CopyN(server, client, pending); err != nil {
			r.Log.Error(err)
			return err
		}

		var err error
		if lastError.Header, err = protocol.ReadHeader(server); err != nil {
			r.Log.Error(err)
			return err
		}
		pending = int64(lastError.Header.MessageLength - protocol.HeaderLen)
		if _, err = io.CopyN(&lastError.Rest, server, pending); err != nil {
			r.Log.Error(err)
			return err
		}
		r.Log.Debugf("caching new getLastError response: %s", lastError.Rest.Bytes())
	} else {
		// We need to discard the pending bytes from the client from the query
		// before we send it our cached response.
		var written int
		for _, b := range parts {
			written += len(b)
		}
		pending := int64(h.MessageLength) - int64(written)
		if _, err := io.CopyN(ioutil.Discard, client, pending); err != nil {
			r.Log.Error(err)
			return err
		}
		// Modify and send the cached response for this request.
		lastError.Header.ResponseTo = h.RequestID
		r.Log.Debugf("using cached getLastError response: %s", lastError.Rest.Bytes())
	}

	if err := lastError.Header.WriteTo(client); err != nil {
		r.Log.Error(err)
		return err
	}
	if _, err := client.Write(lastError.Rest.Bytes()); err != nil {
		r.Log.Error(err)
		return err
	}

	return nil
}

var errRSChanged = errors.New("dvara: replset config changed")

// ProxyMapper maps real mongo addresses to their corresponding proxy
// addresses.
type ProxyMapper interface {
	Proxy(h string) (string, error)
}

// ReplicaStateCompare provides the last ReplicaSetState and allows for
// checking if it has changed as we rewrite/proxy the isMaster &
// replSetGetStatus queries.
type ReplicaStateCompare interface {
	SameRS(o *replSetGetStatusResponse) bool
	SameIM(o *isMasterResponse) bool
}

type responseRewriter interface {
	Rewrite(client io.Writer, server io.Reader) error
}

type replyPrefix [20]byte

var emptyPrefix replyPrefix

// ReplyRW provides common helpers for rewriting replies from the server.
type ReplyRW struct {
	Log Logger `inject:""`
}

// ReadOne reads a 1 document response, from the server, unmarshals it into v
// and returns the various parts.
func (r *ReplyRW) ReadOne(server io.Reader, v interface{}) (*protocol.MessageHeader, replyPrefix, int32, error) {
	h, err := protocol.ReadHeader(server)
	if err != nil {
		r.Log.Error(err)
		return nil, emptyPrefix, 0, err
	}

	if h.OpCode != protocol.OpReply {
		err := fmt.Errorf("readOneReplyDoc: expected op %s, got %s", protocol.OpReply, h.OpCode)
		return nil, emptyPrefix, 0, err
	}

	var prefix replyPrefix
	if _, err := io.ReadFull(server, prefix[:]); err != nil {
		r.Log.Error(err)
		return nil, emptyPrefix, 0, err
	}

	numDocs := protocol.GetInt32(prefix[:], 16)
	if numDocs != 1 {
		err := fmt.Errorf("readOneReplyDoc: can only handle 1 result document, got: %d", numDocs)
		return nil, emptyPrefix, 0, err
	}

	rawDoc, err := protocol.ReadDocument(server)
	if err != nil {
		r.Log.Error(err)
		return nil, emptyPrefix, 0, err
	}

	if err := bson.Unmarshal(rawDoc, v); err != nil {
		r.Log.Error(err)
		return nil, emptyPrefix, 0, err
	}

	return h, prefix, int32(len(rawDoc)), nil
}

// WriteOne writes a rewritten response to the client.
func (r *ReplyRW) WriteOne(client io.Writer, h *protocol.MessageHeader, prefix replyPrefix, oldDocLen int32, v interface{}) error {
	newDoc, err := bson.Marshal(v)
	if err != nil {
		return err
	}

	h.MessageLength = h.MessageLength - oldDocLen + int32(len(newDoc))
	parts := [][]byte{h.ToWire(), prefix[:], newDoc}
	for _, p := range parts {
		if _, err := client.Write(p); err != nil {
			return err
		}
	}

	return nil
}

type isMasterResponse struct {
	Hosts   []string `bson:"hosts,omitempty"`
	Primary string   `bson:"primary,omitempty"`
	Me      string   `bson:"me,omitempty"`
	Extra   bson.M   `bson:",inline"`
}

// IsMasterResponseRewriter rewrites the response for the "isMaster" query.
type IsMasterResponseRewriter struct {
	Log                 Logger              `inject:""`
	ProxyMapper         ProxyMapper         `inject:""`
	ReplyRW             *ReplyRW            `inject:""`
	ReplicaStateCompare ReplicaStateCompare `inject:""`
}

// Rewrite rewrites the response for the "isMaster" query.
func (r *IsMasterResponseRewriter) Rewrite(client io.Writer, server io.Reader) error {
	var err error
	var q isMasterResponse
	h, prefix, docLen, err := r.ReplyRW.ReadOne(server, &q)
	if err != nil {
		return err
	}
	if !r.ReplicaStateCompare.SameIM(&q) {
		return errRSChanged
	}

	var newHosts []string
	for _, h := range q.Hosts {
		newH, err := r.ProxyMapper.Proxy(h)
		if err != nil {
			if pme, ok := err.(*ProxyMapperError); ok {
				if pme.State != ReplicaStateArbiter {
					r.Log.Errorf("dropping member %s in state %s", h, pme.State)
				}
				continue
			}
			// unknown err
			return err
		}
		newHosts = append(newHosts, newH)
	}
	q.Hosts = newHosts

	if q.Primary != "" {
		// failure in mapping the primary is fatal
		if q.Primary, err = r.ProxyMapper.Proxy(q.Primary); err != nil {
			return err
		}
	}
	if q.Me != "" {
		// failure in mapping me is fatal
		if q.Me, err = r.ProxyMapper.Proxy(q.Me); err != nil {
			return err
		}
	}
	return r.ReplyRW.WriteOne(client, h, prefix, docLen, q)
}

type statusMember struct {
	Name  string       `bson:"name"`
	State ReplicaState `bson:"stateStr,omitempty"`
	Self  bool         `bson:"self,omitempty"`
	Extra bson.M       `bson:",inline"`
}

type replSetGetStatusResponse struct {
	Members []statusMember         `bson:"members"`
	Extra   map[string]interface{} `bson:",inline"`
}

// ReplSetGetStatusResponseRewriter rewrites the "replSetGetStatus" response.
type ReplSetGetStatusResponseRewriter struct {
	Log                 Logger              `inject:""`
	ProxyMapper         ProxyMapper         `inject:""`
	ReplyRW             *ReplyRW            `inject:""`
	ReplicaStateCompare ReplicaStateCompare `inject:""`
}

// Rewrite rewrites the "replSetGetStatus" response.
func (r *ReplSetGetStatusResponseRewriter) Rewrite(client io.Writer, server io.Reader) error {
	var err error
	var q replSetGetStatusResponse
	h, prefix, docLen, err := r.ReplyRW.ReadOne(server, &q)
	if err != nil {
		return err
	}
	if !r.ReplicaStateCompare.SameRS(&q) {
		return errRSChanged
	}

	var newMembers []statusMember
	for _, m := range q.Members {
		newH, err := r.ProxyMapper.Proxy(m.Name)
		if err != nil {
			if pme, ok := err.(*ProxyMapperError); ok {
				if pme.State != ReplicaStateArbiter {
					r.Log.Errorf("dropping member %s in state %s", h, pme.State)
				}
				continue
			}
			// unknown err
			return err
		}
		m.Name = newH
		newMembers = append(newMembers, m)
	}
	q.Members = newMembers
	return r.ReplyRW.WriteOne(client, h, prefix, docLen, q)
}
