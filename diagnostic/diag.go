package diagnostic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"crypto/rand"

	"github.com/jbenet/go-ipfs/Godeps/_workspace/src/code.google.com/p/go.net/context"
	"github.com/jbenet/go-ipfs/Godeps/_workspace/src/code.google.com/p/goprotobuf/proto"
	"github.com/op/go-logging"

	net "github.com/jbenet/go-ipfs/net"
	msg "github.com/jbenet/go-ipfs/net/message"
	peer "github.com/jbenet/go-ipfs/peer"
)

var log = logging.MustGetLogger("diagnostics")

type Diagnostics struct {
	network net.Network
	sender  net.Sender
	self    *peer.Peer

	diagLock sync.Mutex
	diagMap  map[string]time.Time
	birth    time.Time
}

func NewDiagnostics(self *peer.Peer, inet net.Network, sender net.Sender) *Diagnostics {
	return &Diagnostics{
		network: inet,
		sender:  sender,
		self:    self,
		diagMap: make(map[string]time.Time),
		birth:   time.Now(),
	}
}

type connDiagInfo struct {
	Latency time.Duration
	ID      string
}

type diagInfo struct {
	ID          string
	Connections []connDiagInfo
	Keys        []string
	LifeSpan    time.Duration
	CodeVersion string
}

func (di *diagInfo) Marshal() []byte {
	b, err := json.Marshal(di)
	if err != nil {
		panic(err)
	}
	//TODO: also consider compressing this. There will be a lot of these
	return b
}

func (d *Diagnostics) getPeers() []*peer.Peer {
	// <HACKY>
	n, ok := d.network.(*net.IpfsNetwork)
	if !ok {
		return nil
	}
	s := n.GetSwarm()
	return s.GetPeerList()
	// </HACKY>
}

func (d *Diagnostics) getDiagInfo() *diagInfo {
	di := new(diagInfo)
	di.CodeVersion = "github.com/jbenet/go-ipfs"
	di.ID = d.self.ID.Pretty()
	di.LifeSpan = time.Since(d.birth)
	di.Keys = nil // Currently no way to query datastore

	for _, p := range d.getPeers() {
		di.Connections = append(di.Connections, connDiagInfo{p.GetLatency(), p.ID.Pretty()})
	}
	return di
}

func newID() string {
	id := make([]byte, 4)
	rand.Read(id)
	return string(id)
}

func (d *Diagnostics) GetDiagnostic(timeout time.Duration) ([]*diagInfo, error) {
	ctx, _ := context.WithTimeout(context.TODO(), timeout)

	diagID := newID()
	d.diagLock.Lock()
	d.diagMap[diagID] = time.Now()
	d.diagLock.Unlock()

	log.Debug("Begin Diagnostic")

	peers := d.getPeers()
	log.Debug("Sending diagnostic request to %d peers.", len(peers))

	var out []*diagInfo
	di := d.getDiagInfo()
	out = append(out, di)

	pmes := newMessage(diagID)
	for _, p := range peers {
		data, err := d.getDiagnosticFromPeer(ctx, p, pmes)
		if err != nil {
			log.Error("GetDiagnostic error: %v", err)
			continue
		}
		buf := bytes.NewBuffer(data)
		dec := json.NewDecoder(buf)
		for {
			di := new(diagInfo)
			err := dec.Decode(di)
			if err != nil {
				if err != io.EOF {
					log.Error("error decoding diagInfo: %v", err)
				}
				break
			}
			out = append(out, di)
		}
	}
	return out, nil
}

// TODO: this method no longer needed.
func (d *Diagnostics) getDiagnosticFromPeer(ctx context.Context, p *peer.Peer, mes *Message) ([]byte, error) {
	rpmes, err := d.sendRequest(ctx, p, mes)
	if err != nil {
		return nil, err
	}
	return rpmes.GetData(), nil
}

func newMessage(diagID string) *Message {
	pmes := new(Message)
	pmes.DiagID = proto.String(diagID)
	return pmes
}

func (d *Diagnostics) sendRequest(ctx context.Context, p *peer.Peer, pmes *Message) (*Message, error) {

	mes, err := msg.FromObject(p, pmes)
	if err != nil {
		return nil, err
	}

	start := time.Now()

	rmes, err := d.sender.SendRequest(ctx, mes)
	if err != nil {
		return nil, err
	}
	if rmes == nil {
		return nil, errors.New("no response to request")
	}

	rtt := time.Since(start)
	log.Info("diagnostic request took: %s", rtt.String())

	rpmes := new(Message)
	if err := proto.Unmarshal(rmes.Data(), rpmes); err != nil {
		return nil, err
	}

	return rpmes, nil
}

// NOTE: not yet finished, low priority
func (d *Diagnostics) handleDiagnostic(p *peer.Peer, pmes *Message) (*Message, error) {
	resp := newMessage(pmes.GetDiagID())
	d.diagLock.Lock()
	_, found := d.diagMap[pmes.GetDiagID()]
	if found {
		d.diagLock.Unlock()
		return resp, nil
	}
	d.diagMap[pmes.GetDiagID()] = time.Now()
	d.diagLock.Unlock()

	buf := new(bytes.Buffer)
	di := d.getDiagInfo()
	buf.Write(di.Marshal())

	ctx, _ := context.WithTimeout(context.TODO(), time.Second*10)

	for _, p := range d.getPeers() {
		out, err := d.getDiagnosticFromPeer(ctx, p, pmes)
		if err != nil {
			log.Error("getDiagnostic error: %v", err)
			continue
		}
		_, err = buf.Write(out)
		if err != nil {
			log.Error("getDiagnostic write output error: %v", err)
			continue
		}
	}

	resp.Data = buf.Bytes()
	return resp, nil
}

func (d *Diagnostics) HandleMessage(ctx context.Context, mes msg.NetMessage) (msg.NetMessage, error) {
	mData := mes.Data()
	if mData == nil {
		return nil, errors.New("message did not include Data")
	}

	mPeer := mes.Peer()
	if mPeer == nil {
		return nil, errors.New("message did not include a Peer")
	}

	// deserialize msg
	pmes := new(Message)
	err := proto.Unmarshal(mData, pmes)
	if err != nil {
		return nil, fmt.Errorf("Failed to decode protobuf message: %v\n", err)
	}

	// Print out diagnostic
	log.Info("[peer: %s] Got message from [%s]\n",
		d.self.ID.Pretty(), mPeer.ID.Pretty())

	// dispatch handler.
	rpmes, err := d.handleDiagnostic(mPeer, pmes)
	if err != nil {
		return nil, err
	}

	// if nil response, return it before serializing
	if rpmes == nil {
		return nil, nil
	}

	// serialize response msg
	rmes, err := msg.FromObject(mPeer, rpmes)
	if err != nil {
		return nil, fmt.Errorf("Failed to encode protobuf message: %v\n", err)
	}

	return rmes, nil
}
