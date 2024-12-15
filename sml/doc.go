// Package sml provides functions for parsing SML (SECS Message Language)
// representations of SECS-II messages, primarily used in the context of HSMS (High-Speed SECS Message Services).
//
// SML is a human-readable text-based format for representing SECS-II messages, which are used for
// communication between host systems and semiconductor manufacturing equipment.
//
// It supports various SECS-II data item types and provides options for handling different
// ASCII parsing modes:
//
// Strict Mode: Adheres to the ASCII printable characters (character codes 32 to 126) and
// supports parsing non-printable ASCII characters represented by their decimal values (e.g., 0x0A for newline).
//
// This is useful when parsing SML generated with strict mode for SECS-II ASCII items (e.g., using secs2.WithASCIIStrictMode(true)).
//
// Non-Strict Mode (default): Optimizes for performance by making certain assumptions about the input:
//
//   - It assumes that the ASCII string does not contain the same quote character as the one used to enclose the ASCII item.
//
//   - It does not handle escape sequences.
//
// Usage Example:
//
//	// Use strict mode
//	sml.WithASCIIStrictMode(true)
//	// Parse an SML string
//	messages, err := sml.ParseHSMS(`
//	    MessageName: 'S1F1' W
//	    <L[1]
//	        <A[4] 'test'>
//	    >
//	    .
//	`)
//	if err != nil {
//	    // Handle error
//	}
//	// Process the parsed HSMS data messages
package sml
