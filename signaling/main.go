package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/pion/webrtc/v4"
)

type Signaling struct {
	port                   string
	upgrader               websocket.Upgrader
	activeConnections      map[*websocket.Conn]bool
	activeConnectionsMutex sync.RWMutex
	activeSubjects         map[*websocket.Conn]string
	activeSubjectsMu       sync.Mutex
	wsMutex                sync.Mutex

	nat *nats.Conn
}

type ClientMessage struct {
	Type      string                    `json:"type"`
	Candidate webrtc.ICECandidateInit   `json:"cadidate,omitempty"`
	Sdp       webrtc.SessionDescription `json:"sdp,omitempty"`
}

// client sends its sdp offer to router node
func (s *Signaling) ClientSendSdpOffer(sdp webrtc.SessionDescription) {

	b, err := json.Marshal(sdp)
	if err != nil {
		log.Printf("Error%s", err)
	}

	subject := "to.router.a.add.sdp"
	err = s.nat.Publish(subject, b)
	if err != nil {
		log.Println("Error publishing client sdp message to NATS:", err)
		// Handle error
	}
}

// client sends its ice as soon as it is found
func (s *Signaling) ClientSendCandidate(ice webrtc.ICECandidateInit) {
	b, err := json.Marshal(ice)
	if err != nil {
		log.Printf("Error%s", err)
	}

	s.nat.Publish("to.router.a.add.candidate", b)
	if err != nil {
		log.Println("Error publishing client ice message to NATS:", err)
		// Handle error
	}
}

func (s *Signaling) handleWs(w http.ResponseWriter, r *http.Request) {
	log.Println(">>>>>>>>>>>>>>>> need to be called once ")
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	s.activeConnectionsMutex.Lock()
	s.activeConnections[conn] = true
	s.activeConnectionsMutex.Unlock()

	defer delete(s.activeConnections, conn)

	// Create channels to signal completion of subscription callbacks
	doneCh := make(chan struct{}, 2)

	// Subscribe to listen for SDP answer
	go func() {
		subject := "to.client.a.send.sdp.answer"
		sub, err := s.nat.Subscribe(subject, func(msg *nats.Msg) {
			log.Printf("subscribe --------------------> %s/n", subject)
			s.ForwardAnswer(conn, msg)
			doneCh <- struct{}{} // Signal completion
		})
		if err != nil {
			log.Printf("unable to create subscription %s : %s", subject, err)
		}
		defer sub.Unsubscribe()
		<-doneCh // Wait for completion
	}()

	// Subscribe to listen for ICE candidate
	go func() {
		subject := "to.client.a.send.candidate"
		sub, err := s.nat.Subscribe(subject, func(msg *nats.Msg) {
			fmt.Printf("ice received : %+v", string(msg.Data))
			s.ForwardCandidate(conn, msg)
			doneCh <- struct{}{} // Signal completion
		})
		if err != nil {
			log.Printf("unable to create subscription %s : %s", subject, err)
		}
		defer sub.Unsubscribe()
		<-doneCh // Wait for completion
	}()

	// Read messages from WebSocket connection
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			break
		}

		// Unmarshal each inbound WebSocket message
		var (
			candidate webrtc.ICECandidateInit
			offer     webrtc.SessionDescription
		)

		switch {
		// Attempt to unmarshal as a SessionDescription. If the SDP field is empty
		// assume it is not one.
		case json.Unmarshal(msg, &offer) == nil && offer.SDP != "":
			// Send SDP
			s.ClientSendSdpOffer(offer)
		case json.Unmarshal(msg, &candidate) == nil && candidate.Candidate != "":
			// Send ICE
			s.ClientSendCandidate(candidate)
		default:
			panic("Unknown message")
		}
	}

	s.activeConnectionsMutex.Lock()
	delete(s.activeConnections, conn)
	s.activeConnectionsMutex.Unlock()
}

func (s *Signaling) handleWsOld(w http.ResponseWriter, r *http.Request) {
	log.Println(">>>>>>>>>>>>>>>> need to be called once ")
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	s.activeConnectionsMutex.Lock()
	s.activeConnections[conn] = true
	s.activeConnectionsMutex.Unlock()

	defer delete(s.activeConnections, conn)

	//signal subscripes to listen sdp answer
	subject := "to.client.a.send.sdp.answer"
	sub, err := s.nat.Subscribe(subject, func(msg *nats.Msg) {
		log.Printf("subscribe --------------------> %s/n", subject)
		s.ForwardAnswer(conn, msg)
	})
	if err != nil {
		log.Printf("unable to create subscription %s : %s", subject, err)
	}
	defer sub.Unsubscribe()

	//signal subscripes to listen candidate
	subject = "to.client.a.send.candidate"
	sub, err = s.nat.Subscribe(subject, func(msg *nats.Msg) {
		fmt.Printf("ice received : %+v", string(msg.Data))
		s.ForwardCandidate(conn, msg)
	})
	if err != nil {
		log.Printf("unable to create subscription %s : %s", subject, err)
	}
	defer sub.Unsubscribe()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			break
		}

		// Unmarshal each inbound WebSocket message
		var (
			candidate webrtc.ICECandidateInit
			offer     webrtc.SessionDescription
		)

		switch {
		// Attempt to unmarshal as a SessionDescription. If the SDP field is empty
		// assume it is not one.
		case json.Unmarshal(msg, &offer) == nil && offer.SDP != "":
			//send sdp
			s.ClientSendSdpOffer(offer)
		case json.Unmarshal(msg, &candidate) == nil && candidate.Candidate != "":
			//send ice
			s.ClientSendCandidate(candidate)
		default:
			panic("Unknown message")
		}
	}

	s.activeConnectionsMutex.Lock()
	delete(s.activeConnections, conn)
	s.activeConnectionsMutex.Unlock()
}

func (s *Signaling) sendMessage(sender *websocket.Conn, msg []byte) {
	s.wsMutex.Lock()
	defer s.wsMutex.Unlock()
	for conn := range s.activeConnections {
		if conn == sender {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				log.Println(err)
				continue
			}
		}
	}
}

func main() {

	s := &Signaling{
		activeConnections: make(map[*websocket.Conn]bool),
		activeSubjects:    make(map[*websocket.Conn]string),

		port: ":3111",
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,

			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}

	s.NatCon()

	http.HandleFunc("/ws", s.handleWs)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./webclient/index.html")
	})

	http.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./webclient/consumer.html")
	})

	log.Printf("test web client running on port %s", s.port)
	log.Fatal(http.ListenAndServe(s.port, nil))
}

func (s *Signaling) ForwardAnswer(conn *websocket.Conn, msg *nats.Msg) {
	//forward to client
	s.sendMessage(conn, msg.Data)
}

func (s *Signaling) ForwardCandidate(conn *websocket.Conn, msg *nats.Msg) {
	//forward to client

	s.sendMessage(conn, msg.Data)
}

func (s *Signaling) NatCon() {
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://localhost:4222"
	}

	nc, err := nats.Connect(url)
	if err != nil {
		log.Printf("Error: unbale to connect to NATS server %v", err)
	}

	log.Printf("Connected to NATS server at %s", url)

	// Close the NATS connection when done
	//defer nc.Close()

	s.nat = nc
}
