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

func logHandler(msg *hsms.DataMessage, session hsms.Session) {
	// logger.Info("Log message", "id", msg.ID(), "sml", msg.ToSML())
}

func msgHandler(msg *hsms.DataMessage, session hsms.Session) {
	log := logger.GetLogger()

	log.Info("Handle message", "id", msg.ID(), "sml", msg.ToSML())
	if msg.StreamCode() != 9 {
		item := secs2.A("reply")
		_ = session.ReplyDataMessage(msg, item)
	} else {
		log.Info(fmt.Sprintf("Receive S%dF%d", msg.StreamCode(), msg.FunctionCode()))
	}
}

func connChangeHandler(conn hsms.Connection, prevState hsms.ConnState, newState hsms.ConnState) {
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
	session.AddDataMessageHandler(msgHandler, logHandler)
	session.AddConnStateChangeHandler(connChangeHandler)

	go func() {
		err := conn.Open(true)
		if err != nil {
			log.Error("EQP: failed to open equipment", "error", err)
			os.Exit(1)
		}
	}()

	exitSig := make(chan os.Signal, 1)
	signal.Notify(exitSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	<-exitSig

	log.Info("EQP: exit signal received")
	conn.Close()
	cancel()

	log.Info("EQP: Shutdown finished")
}
