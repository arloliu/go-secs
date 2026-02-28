package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
)

type passiveHost struct {
	ctx     context.Context
	session hsms.Session
}

func (h *passiveHost) connChangeHandler(conn hsms.Connection, prevState hsms.ConnState, newState hsms.ConnState) {
	if newState.IsSelected() {
		go h.sendMsgLoop()
	}
}

func (h *passiveHost) sendMsgLoop() {
	i := 0
	for {
		select {
		case <-h.ctx.Done():
			return
		default:
			i++
			item := secs2.A(fmt.Sprintf("data %d", i))
			replyMsg, err := h.session.SendDataMessage(1, 3, true, item)
			if err != nil {
				logger.Error("failed to send S1F3 message", "error", err)
			} else {
				replyMsg.Free()
				replyMsg.Free()
				logger.Info("S1F3 reply received, sleep 1 sec", "sml", replyMsg.ToSML())
			}
			time.Sleep(1000 * time.Millisecond)
		}
	}
}

func (h *passiveHost) dataMsgHandler(msg *hsms.DataMessage, session hsms.Session) {
	log := logger.GetLogger()

	log.Info("Handle message", "id", msg.ID(), "sml", msg.ToSML())
	if msg.StreamCode() != 9 {
		item := secs2.A("reply")
		_ = session.ReplyDataMessage(msg, item)
	} else {
		log.Info(fmt.Sprintf("Receive S%dF%d", msg.StreamCode(), msg.FunctionCode()))
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if os.Getenv("ENV") == "development" {
		logger.SetLevel(logger.DebugLevel)
	} else {
		logger.SetLevel(logger.InfoLevel)
	}

	log := logger.GetLogger()

	var sessionID uint16 = 10
	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", 6000,
		hsmsss.WithPassive(),
		hsmsss.WithT3Timeout(10*time.Second),
		hsmsss.WithT5Timeout(1000*time.Millisecond),
		hsmsss.WithT8Timeout(2*time.Second),
		hsmsss.WithConnectRemoteTimeout(1*time.Second),
		hsmsss.WithCloseConnTimeout(5*time.Second),
	)
	if err != nil {
		fmt.Printf("EQP error: %v\n", err)
		return
	}

	conn, err := hsmsss.NewConnection(ctx, cfg)
	if err != nil {
		fmt.Printf("EQP error: %v\n", err)
		return
	}

	session := conn.AddSession(sessionID)

	host := &passiveHost{ctx: ctx, session: session}

	session.AddDataMessageHandler(host.dataMsgHandler)
	session.AddConnStateChangeHandler(host.connChangeHandler)

	err = conn.Open(false)
	if err != nil {
		logger.Error("EQP: failed to open equipment", "error", err)
		return
	}

	exitSig := make(chan os.Signal, 1)
	signal.Notify(exitSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	<-exitSig

	log.Info("EQP: exit signal received")
	conn.Close()
	cancel()

	logger.Info("EQP: Shutdown finished")
}
