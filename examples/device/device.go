// The simple example of HSMS-SS communication.
//
// It creates a device and send request message per second.
//
// The mode can be configurated as active of passive, and role can be configured as host or equipment
// by environment variables.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
)

var log logger.Logger

func connStateChangeHandler(conn hsms.Connection, prevState hsms.ConnState, newState hsms.ConnState) {
	log.Info("connection state changed", "prevState", prevState, "newState", newState)
}

func dataMsgHandler(msg *hsms.DataMessage, session hsms.Session) {
	log.Info("receive message",
		"id", msg.ID(),
		"streamCode", msg.StreamCode(),
		"functionCode", msg.FunctionCode(),
		"waitBit", msg.WaitBit(),
		"item", msg.Item().ToSML(),
	)
	switch msg.StreamCode() { //nolint:gocritic
	case 99:
		switch msg.FunctionCode() {
		case 1, 3, 5:
			err := session.ReplyDataMessage(msg, msg.Item())
			if err != nil {
				log.Error("failed to reply message", "id", msg.ID())
			}
		}
	}
}

func main() {
	ip := "127.0.0.1"
	port := 5000
	var sessionID uint16 = 1000

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	os.Setenv("ENV", "development")
	log = logger.NewSlog(logger.InfoLevel, false)

	opts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(10 * time.Second),
		hsmsss.WithT5Timeout(1 * time.Second),
		hsmsss.WithT8Timeout(2 * time.Second),
		hsmsss.WithConnectRemoteTimeout(1 * time.Second),
		hsmsss.WithLinktestInterval(10 * time.Second),
	}

	if os.Getenv("MODE") == "passive" {
		opts = append(opts, hsmsss.WithPassive())
	} else {
		opts = append(opts, hsmsss.WithActive())
	}

	if os.Getenv("ROLE") == "eqp" {
		opts = append(opts, hsmsss.WithEquipRole())
	} else {
		opts = append(opts, hsmsss.WithHostRole())
	}

	if val := os.Getenv("IP"); val != "" {
		ip = val
	}

	if val := os.Getenv("PORT"); val != "" {
		if n, err := strconv.Atoi(val); err != nil {
			port = n
		}
	}
	if val := os.Getenv("SESSION_ID"); val != "" {
		if n, err := strconv.ParseUint(val, 10, 16); err != nil {
			sessionID = uint16(n)
		}
	}

	cfg, err := hsmsss.NewConnectionConfig(ip, port, opts...)
	if err != nil {
		log.Error("failed to create connection config", "error", err)
		return
	}

	conn, err := hsmsss.NewConnection(ctx, cfg)
	if err != nil {
		log.Error("failed to create connection", "error", err)
		return
	}

	session := conn.AddSession(sessionID)

	session.AddConnStateChangeHandler(connStateChangeHandler)
	session.AddDataMessageHandler(dataMsgHandler)

	go func() {
		err := conn.Open(true)
		if err != nil {
			log.Error("failed to open connection", "error", err)
			return
		}

		i := 0

	reqLoop:
		for {
			select {
			case <-ctx.Done():
				break reqLoop
			default:
				i++
				item := secs2.A(fmt.Sprintf("data %d", i))
				replyMsg, err := session.SendDataMessage(99, 1, true, item)
				if err != nil {
					log.Error("failed to send S99F1 message", "error", err)
				} else {
					replyMsg.Free()
					log.Info("reply received, sleep 1 sec",
						"streamCode", replyMsg.StreamCode(),
						"functionCode", replyMsg.FunctionCode(),
						"sml", replyMsg.ToSML(),
					)
				}
				time.Sleep(1000 * time.Millisecond)
			}
		}
	}()

	exitSig := make(chan os.Signal, 1)
	signal.Notify(exitSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	<-exitSig

	log.Info("exit signal received")

	conn.Close()
	cancel()

	log.Info("shutdown finished")
}
