# go-secs

This project is a library that implements [SECS-II](https://en.wikipedia.org/wiki/SECS-II)/[HSMS](https://en.wikipedia.org/wiki/High-Speed_SECS_Message_Services) in Go.

![Test Status](https://github.com/arloliu/go-secs/actions/workflows/ci.yaml/badge.svg)
[![Go Reference](https://pkg.go.dev/badge/github.com/arloliu/go-secs.svg)](https://pkg.go.dev/github.com/arloliu/go-secs)
[![Go Report Card](https://goreportcard.com/badge/github.com/arloliu/go-secs)](https://goreportcard.com/report/github.com/arloliu/go-secs)

## Supports

* SECS-II (SEMI-E5)
* HSMS(SEMI-E37), HSMS-SS(SEMI-E37.1)
* SML(supports various formats, single-quoted stream-function, optional message name, single/double-quoted ASCII item,....etc.)

## Features

### SECS-II Operations

* **Comprehensive Data Item Support:** Provides a wide range of data structures to represent various SECS-II data item types, including integers, floating-point numbers, booleans, lists, and ASCII strings.
* **Message Formatting:** Handles the formatting of SECS-II messages, including the header and data item encoding.
* **Serialization:**  Serializes HSMS messages into their byte representation for transmission using the `ToBytes` method.
* **SML Generation:** Generates SML (SECS Message Language) representations of SECS-II messages using the `ToSML` method. Supports various SML formats, including single or double quotes, and handles non-printable ASCII characters.

### HSMS Operations

* **Message Createion:**  Establishes  HSMS control and data message.
* **Message Decoding:** Decodes HSMS control and data messages.
* **Session Management:**  Defines a generic interface for HSMS session.
* **Connection State Management:** Tracks the connection state and handles state transitions.

### HSMS-SS Communication

* **Simplified Interface:** Provides a streamlined interface specifically for HSMS-SS (Single Session) communication.
* **Connection Management:**  Establishes and manages HSMS connections, supporting both active and passive modes.
* **Message Exchange:** Sends and receives HSMS messages, including data messages and control messages.
* **Session Management:**  Handles communication sessions, including session establishment and termination.
* **Data Message Handling:**  Supports registering handlers for processing received data messages.
* **Error Handling:** Provides robust error handling and reporting mechanisms.
* **Concurrent Operations:** Utilizes goroutines and channels for efficient concurrent message sending and receiving.
* **Customization:** Offers configurable options for various HSMS parameters, such as timeouts, linktest behavior, and connection settings.

### SML Operations

* **Parsing:** Parses SML strings into HSMS data messages.
* **Quoting and Escaping:** Supports various SML formats, including different quoting styles for stream and function codes and handling of non-printable ASCII characters and escape sequences.
* **Strict Mode:** Offers a strict parsing mode that adheres to the ASCII standard and handles escape characters literally.

## Sample Usage
## Installation
```bash
go get github.com/arloliu/go-secs
```

### Create and use SECS-II data items
```go
// create a list item
var listItem secs2.Item
listItem = secs2.NewListItem(
    // indices: 0
    secs2.NewASCIIItem("test1"),
    // indices: 1
    secs2.NewIntItem(4, 1, 2, 3, 4), // I4/int32 item with 4 values
    // indices: 2
    secs2.NewIntItem(8, int32[]{1, 2, 3, 4}), // I8/int64 item with 4 values, accepts slice as input
    // indices: 3
    secs2.NewListItem( // nested list item
        // indices: 3, 0
        secs2.NewASCIIItem("test2"),
        // indices: 3, 1
        secs2.NewASCIIItem("test2"),
    ),
)

// or, create liste item with shortcut function
listItem = secs2.L(
    secs2.A("test1"),
    secs2.I4(1, 2, 3, 4), // I4/int32 item with 4 values
    secs2.I8(int64[]{1, 2, 3, 4}), // I8/int64 item with 4 values, accepts slice as input
    secs2.L( // nested list item
        secs2.A("test2"),
        secs2.A("test3"),
    ),
)

// Accessing items
item := listItem.Get(0) // "test"
if item.IsASCII() { // should be true
    str, err := item.ToASCII() // str == "test1", err == nil
}

// get items in the nested list
item, err := listItem.Get(3) // nested list item
if item.IsList() { // should be true
    nestItem, err := item.Get(0) // ascii item "test2"
    nestItem, err = item.Get(1) // ascii item "test3"
    nestItem, err = item.Get(2) // nil, err == "failed to get nested item"
}

// or, use multiple indices to get nested items
nestItem, err := listItem.Get(3, 0) // ascii item "test2"
nestItem, err = listItem.Get(3, 1) // ascii item "test3"
nestItem, err = listItem.Get(3, 2) // nil, err == "failed to get nested item"
```

### Create HSMS-SS Host with Active Mode
```go
func msgHandler(msg *hsms.DataMessage, session hsms.Session) {
	switch msg.StreamCode() {

	}
	session.ReplyDataMessage(msg, msg.Item())
	// ... handle error ...
}

func main() {
	// ...
	// Create a new HSMS-SS connection
	connCfg := hsmsss.NewConnectionConfig("127.0.0.1", 5000,
	WithActive(), // active mode
	WithHostRole(), // host role
	WithT3Timeout(30*time.Second),
	// other options...
	)
	conn, err := hsmsss.NewConnection(ctx, connCfg)
	if err != nil {
		// ... handle error ...
	}
	defer conn.Close()

	// Add a session with session id 1000 before open connection
	session := conn.AddSession(1000)

	// Add a data message handler
	session.AddDataMessageHandler(msgHandler)

	// Open connection and wait it to selected state
	err = conn.Open(true)
	if err != nil {
		// ... handle error ...
	}

	// Send an S1F1 message
	reply, err := session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("test"))
	if err != nil {
		// ... handle error ...
	}
	// Process the reply

	// ... other HSMS-SS operations ...
```