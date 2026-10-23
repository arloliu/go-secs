package gem

import "strconv"

// alcdCategoryNames holds the name of each ALCD category constant, indexed by the category.
// Category 0 has no constant, so its entry is empty.
var alcdCategoryNames = [...]string{
	ALCDPersonalSafety:          "ALCDPersonalSafety",
	ALCDEquipmentSafety:         "ALCDEquipmentSafety",
	ALCDParameterControlWarning: "ALCDParameterControlWarning",
	ALCDParameterControlError:   "ALCDParameterControlError",
	ALCDIrrecoverableError:      "ALCDIrrecoverableError",
	ALCDEquipmentStatusWarning:  "ALCDEquipmentStatusWarning",
	ALCDAttentionFlags:          "ALCDAttentionFlags",
	ALCDDataIntegrity:           "ALCDDataIntegrity",
}

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

// String returns a's category as its constant's name, such as "ALCDPersonalSafety",
// prefixed with "ALCDAlarmSet|" when the set bit is 1,
// so an alarm set on category 1 reads "ALCDAlarmSet|ALCDPersonalSafety".
// A category with no constant reads "ALCD(n)", where n is the category without the set bit,
// such as "ALCD(9)" for 9 and "ALCDAlarmSet|ALCD(0)" for the set bit alone.
func (a ALCD) String() string {
	category := a.Category()

	var name string
	if int(category) < len(alcdCategoryNames) {
		name = alcdCategoryNames[category]
	}

	if name == "" {
		name = "ALCD(" + strconv.Itoa(int(category)) + ")"
	}

	if a.Set() {
		return "ALCDAlarmSet|" + name
	}

	return name
}
