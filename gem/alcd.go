package gem

// Set reports whether a reports an alarm set rather than cleared:
// bit 8, [ALCDAlarmSet], is 1.
func (a ALCD) Set() bool {
	return a&ALCDAlarmSet != 0
}

// Category returns the alarm category a carries in bits 7 to 1, without the set bit:
// one of the ALCD category constants, such as [ALCDPersonalSafety],
// or a reserved or equipment-defined category above [ALCDDataIntegrity].
// E5 defines no category 0.
func (a ALCD) Category() ALCD {
	return a &^ ALCDAlarmSet
}
