// Package hsmsss provides an implementation of HSMS-SS (High-Speed SECS Message Services - Single Session)
// for communication with semiconductor manufacturing equipment according to the SEMI E37 standard.
// It builds upon the core HSMS functionalities provided by the hsms package and offers a simplified
// interface for managing single-session HSMS connections.
//
// Key Features:
//   - HSMS-SS Connection Management: Establishes and manages a single-session HSMS connection with a remote device.
//   - Message Handling: Sends and receives HSMS messages, including data messages and control messages.
//   - State Management: Tracks the connection state and handles state transitions.
//   - Error Handling: Provides robust error handling and reporting.
//   - Concurrent Operations: Utilizes goroutines and channels for concurrent message sending and receiving.
//   - Customization: Offers configuration options for various HSMS parameters like timeouts and linktest behavior.
//
// Connection Establishment:
//   - Create a ConnectionConfig struct with desired configuration parameters using `NewConnectionConfig()`.
//   - Use the `NewConnection` function to create a new HSMS-SS connection.
//   - Call the `Open` method to establish the connection.
//
// Message Sending:
//   - Add a session to the connection using `conn.AddSession(sessionID)`.
//   - Use the `SendMessage`, `SendSECS2Message`, or `SendDataMessage` methods of the Session to send messages
//     to the remote device.
//
// Message Receiving:
//   - Use the `AddDataMessageHandler` method in Session to register a data message handler for processing received data messages.
//
// Connection Termination:
//   - Call the `Close` method to gracefully close the connection.
//   - The `closeConn` method handles the shutdown process, including canceling the context, stopping tasks,
//     and closing the TCP connection.
//
// Usage Example:
//
//	func echoHandler(msg *hsms.DataMessage, session hsms.Session) {
//	    err := session.ReplyDataMessage(msg, msg.Item())
//	    // ... handle error ...
//	}
//
//	func main() {
//	    // ...
//	    // Create a new HSMS-SS connection
//	    connCfg := hsmsss.NewConnectionConfig("127.0.0.1", 5000,
//	        WithActive(), // active mode
//	        WithHostRole(), // host role
//	        WithT3Timeout(30*time.Second),
//	        // other options...
//	    )
//	    conn, err := hsmsss.NewConnection(ctx, connCfg)
//	    // ... handle error ...
//	    defer conn.Close()
//
//	    // Add a session with session id 1000 before open connection
//	    session := conn.AddSession(1000)
//
//	    // Add a data message handler
//	    session.AddDataMessageHandler(echoHandler)
//
//	    // Open connection and wait it to selected state
//	    err = conn.Open(true)
//	    // ... handle error ...
//
//	    // Send an S1F1 message
//	    reply, err := session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("test"))
//	    // ... handle error ...
//	    // Process the reply
//
//	    // ... other HSMS-SS operations ...
//	}
package hsmsss
