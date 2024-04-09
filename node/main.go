package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"

	"github.com/nats-io/nats.go"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v4"
)

type RouterNode struct {
	nat            *nats.Conn
	peerConnection *webrtc.PeerConnection
	outputTrack    *webrtc.TrackLocalStaticRTP

	pendingCandidates []*webrtc.ICECandidate
	candidatesMux     sync.Mutex

	remotePendingCandidates []webrtc.ICECandidateInit
	remoteCandidatesMux     sync.Mutex
}

func main() {
	n := &RouterNode{
		pendingCandidates: make([]*webrtc.ICECandidate, 0),
	}

	n.NatCon()

	n.PeerConnection()

	//regisgter peerConnection events

	n.peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		n.onVideoTrackArrived(track, receiver)
	})

	n.peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		n.onStateChange(s)
	})

	n.peerConnection.OnICEConnectionStateChange(func(ic webrtc.ICEConnectionState) {
		n.onIceStateChange(ic)
	})

	n.peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		n.onIceCandidate(c)
	})

	outputTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:     webrtc.MimeTypeVP8,
		ClockRate:    90000,
		SDPFmtpLine:  "max-fs=12288;max-fr=60",
		RTCPFeedback: nil,
	}, "video_"+strconv.FormatUint(7777, 10), strconv.FormatUint(7777, 10))
	if err != nil {
		log.Printf("Error%s", err)
	}

	_, err = n.peerConnection.AddTrack(outputTrack)
	if err != nil {
		log.Printf("Error%s", err)
	}
	n.outputTrack = outputTrack

	// Router subscribes to listen for SDP offers
	subject := "to.router.a.add.sdp"
	sub, err := n.nat.Subscribe(subject, func(msg *nats.Msg) {
		go func() {
			n.AddSdp(msg)
		}()
	})
	if err != nil {
		log.Printf("Unable to create subscription %s: %s", subject, err)
	}
	defer sub.Unsubscribe()

	// Router subscribes to listen for ICE candidates
	subject = "to.router.a.add.candidate"
	sub, err = n.nat.Subscribe(subject, func(msg *nats.Msg) {
		go func() {
			n.AddCandidate(msg)
		}()
	})
	if err != nil {
		log.Printf("Unable to create subscription %s: %s", subject, err)
	}
	defer sub.Unsubscribe()

	fmt.Printf("router node is running")
	select {}
}

// important remote candidates will arrive before sdp offer so store in pending
func (n *RouterNode) AddCandidate(msg *nats.Msg) {
	candidate := webrtc.ICECandidateInit{}
	err := json.Unmarshal(msg.Data, &candidate)
	if err != nil {
		log.Printf("Error unmarshal %s", err)
	}

	//* important
	desc := n.peerConnection.RemoteDescription()
	if desc == nil {
		n.remoteCandidatesMux.Lock()
		n.remotePendingCandidates = append(n.remotePendingCandidates, candidate)
		n.remoteCandidatesMux.Unlock()
		return
	}

	//check if we have early arrived candidats
	if len(n.remotePendingCandidates) > 0 {
		// add it
		n.remoteCandidatesMux.Lock()
		for _, ci := range n.remotePendingCandidates {
			err = n.peerConnection.AddICECandidate(ci)
			if err != nil {
				log.Printf("Error adding candidate %s", err)
			}

		}
		n.remoteCandidatesMux.Unlock()
	}

	// these candidate arrived on time (after the sdp offer)
	err = n.peerConnection.AddICECandidate(candidate)
	if err != nil {
		log.Printf("Error adding candidate %s", err)
	}

	log.Printf("candidate added %s", err)
}

func (n *RouterNode) AddSdp(msg *nats.Msg) {
	sdp := webrtc.SessionDescription{}

	err := json.Unmarshal(msg.Data, &sdp)
	if err != nil {
		log.Printf("Error unmarshal %s", err)
		return // Terminate processing if unmarshal fails
	}

	if err := n.peerConnection.SetRemoteDescription(sdp); err != nil {
		panic(err)
	}

	// Create an answer to send to the other process
	answer, err := n.peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Sets the LocalDescription, and starts our UDP listeners
	if err := n.peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	// Send the answer to signaling -> web client
	payload, err := json.Marshal(answer)
	if err != nil {
		panic(err)
	}

	subject := "to.client.a.send.sdp.answer"
	if err := n.nat.Publish(subject, payload); err != nil {
		log.Printf("Error publishing subject %s: %s", subject, err)
		// Handle error
	}

	log.Println("AddSdp process completed")
}

func (n *RouterNode) NatCon() {
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://localhost:4222"
	}

	nc, err := nats.Connect(url)
	if err != nil {
		log.Printf("Error: unbale to connect to NATS server %v", err)
		os.Exit(0)
	}

	log.Printf("Connected to NATS server at %s", url)

	// Close the NATS connection when done
	//defer nc.Close()

	n.nat = nc
}

func (n *RouterNode) PeerConnection() {
	m := &webrtc.MediaEngine{}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	i := &interceptor.Registry{}

	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		panic(err)
	}
	i.Add(intervalPliFactory)

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
			{
				URLs:       []string{"turn:turn.webrtcwire.online"},
				Username:   "webrtcwire",
				Credential: "yellowgreen",
			},
		},
	}

	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	// defer func() {
	// 	if cErr := peerConnection.Close(); cErr != nil {
	// 		fmt.Printf("cannot close peerConnection: %v\n", cErr)
	// 	}
	// }()

	n.peerConnection = peerConnection
}

func (n *RouterNode) onStateChange(s webrtc.PeerConnectionState) {
	log.Printf("peer connection state has changed: %s\n", s.String())
	switch s {
	case webrtc.PeerConnectionStateConnected:
	case webrtc.PeerConnectionStateFailed:
	case webrtc.PeerConnectionStateDisconnected:
		fallthrough
	case webrtc.PeerConnectionStateClosed:
	}
}

func (n *RouterNode) onIceStateChange(ic webrtc.ICEConnectionState) {
	log.Printf("peer ICE state has changed: %s\n", ic.String())

	switch ic {
	case webrtc.ICEConnectionStateConnected:
		log.Println("✅️ ICE connected stream ")
	case webrtc.ICEConnectionStateCompleted:
	case webrtc.ICEConnectionStateClosed:
	}
}

// When an ICE candidate is available send to the other Pion instance
func (n *RouterNode) onIceCandidate(c *webrtc.ICECandidate) {

	if c == nil {
		return
	}

	n.candidatesMux.Lock()
	defer n.candidatesMux.Unlock()

	//check if remote description is set and start sending candidates
	desc := n.peerConnection.RemoteDescription()
	if desc == nil {
		n.pendingCandidates = append(n.pendingCandidates, c)
	} else {
		payload, err := json.Marshal(c.ToJSON())
		if err != nil {
			log.Printf("Error%s", err)
		}
		subject := "to.client.a.send.candidate"
		err = n.nat.Publish(subject, payload)
		if err != nil {
			log.Printf("Error publishing subject %s: %s", subject, err)
			// Handle error
		}
	}
	log.Println("candidate added")
}

func (n *RouterNode) onVideoTrackArrived(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	log.Printf("🎞️  webrtc stream has started, of type %d: %s\n", track.PayloadType(), track.Codec().MimeType)

	for {
		// Read RTP packets being sent to Pion
		rtp, _, readErr := track.ReadRTP()
		if readErr != nil {
			panic(readErr)
		}

		if writeErr := n.outputTrack.WriteRTP(rtp); writeErr != nil {
			panic(writeErr)
		}
	}
}
