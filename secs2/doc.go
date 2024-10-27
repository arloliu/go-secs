// Package secs2 provides data structures and functions for working with SECS-II messages,
// the communication protocol used in the semiconductor manufacturing industry.
// It's designed to be used in conjunction with the HSMS (High-Speed SECS Message Services) protocol,
// which defines the transport layer for SECS-II messages.
//
// Key Features:
//   - Data Item Representation: Provides a comprehensive set of data structures to represent
//     various SECS-II data item types (e.g., integers, floating-point numbers, booleans, lists, ASCII strings).
//   - Message Formatting:  Handles the formatting of SECS-II messages, including the header and data item encoding.
//   - SML Support:  Includes functions for generating SML (SECS Message Language), a
//     human-readable text representation of SECS-II messages.
//   - Strict Mode for ASCII item: Offers an option to enable strict mode for parsing and generating ASCII data items,
//     ensuring adherence to the ASCII standard.
//
// Usage Example:
//
//	// Create a list item
//	listItem := secs2.NewListItem(
//	    secs2.NewIntItem(4, 1, 2, 3),
//	    secs2.NewASCIIItem("hello"),
//	)
//
//	// Create a S1F1 data message
//	dataMsg, _ := hsms.NewDataMessage(1, 1, true, 10, []byte{1, 2, 3, 4}, listItem)
//
//	// Encode the message to bytes
//	encodedMsg := dataMsg.ToBytes()
//
//	// ... send the encoded message over an HSMS connection .
//
//	// Get SML representation of list item
//	sml := listItem.ToSML()
package secs2
