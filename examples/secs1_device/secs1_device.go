// Example SECS-I over TCP/IP communication.
//
// It creates a SECS-I device (host or equipment) and sends a request message per second.
//
// The mode can be configured as active or passive, and role can be configured
// as host or equipment by environment variables.
//
// Environment variables:
//
//	MODE       - "active" (default) or "passive"
//	ROLE       - "host" (default) or "eqp"
//	IP         - bind/connect address (default: "127.0.0.1")
//	PORT       - TCP port (default: 5000)
//	DEVICE_ID  - SECS-I device ID (default: 1)
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
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs1"
	"github.com/arloliu/go-secs/secs2"
)

var log logger.Logger

func connStateChangeHandler(_ hsms.Connection, prevState hsms.ConnState, newState hsms.ConnState) {
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

	// Echo reply for primary messages (odd function code) that are not stream 9.
	if msg.FunctionCode()%2 == 1 && msg.StreamCode() != 9 {
		if err := session.ReplyDataMessage(msg, msg.Item()); err != nil {
			log.Error("failed to reply message", "id", msg.ID(), "error", err)
		}
	}
}

func main() {
	ip := "127.0.0.1"
	port := 5000
	var deviceID uint16 = 1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log = logger.NewSlog(logger.InfoLevel, false)

	opts := []secs1.ConnOption{
		secs1.WithT3Timeout(10 * time.Second),
		secs1.WithT4Timeout(10 * time.Second),
		secs1.WithConnectTimeout(2 * time.Second),
		secs1.WithSendTimeout(2 * time.Second),
	}

	if os.Getenv("MODE") == "passive" {
		opts = append(opts, secs1.WithPassive())
	} else {
		opts = append(opts, secs1.WithActive())
	}

	if os.Getenv("ROLE") == "eqp" {
		opts = append(opts, secs1.WithEquipRole())
	} else {
		opts = append(opts, secs1.WithHostRole())
	}

	if val := os.Getenv("IP"); val != "" {
		ip = val
	}

	if val := os.Getenv("PORT"); val != "" {
		if n, err := strconv.Atoi(val); err == nil {
			port = n
		}
	}

	if val := os.Getenv("DEVICE_ID"); val != "" {
		if n, err := strconv.ParseUint(val, 10, 16); err == nil {
			deviceID = uint16(n)
		}
	}

	opts = append(opts, secs1.WithDeviceID(deviceID))

	cfg, err := secs1.NewConnectionConfig(ip, port, opts...)
	if err != nil {
		log.Error("failed to create connection config", "error", err)
		return
	}

	conn, err := secs1.NewConnection(ctx, cfg)
	if err != nil {
		log.Error("failed to create connection", "error", err)
		return
	}

	session := conn.AddSession(deviceID)

	session.AddConnStateChangeHandler(connStateChangeHandler)
	session.AddDataMessageHandler(dataMsgHandler)

	go func() {
		if err := conn.Open(true); err != nil {
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
				item := secs2.A(fmt.Sprintf("secs1 data %d", i))
				replyMsg, err := session.SendDataMessage(99, 1, true, item)
				if err != nil {
					log.Error("failed to send S99F1 message", "error", err)
				} else {
					log.Info("reply received",
						"streamCode", replyMsg.StreamCode(),
						"functionCode", replyMsg.FunctionCode(),
						"sml", replyMsg.ToSML(),
					)
				}
				time.Sleep(time.Second)
			}
		}
	}()

	exitSig := make(chan os.Signal, 1)
	signal.Notify(exitSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	<-exitSig

	log.Info("exit signal received")

	_ = conn.Close()
	cancel()

	log.Info("shutdown finished")
}
