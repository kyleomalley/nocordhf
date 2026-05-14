package meshcore

// RadioPreset is a named (region/configuration, freq/bw/sf/cr/tx-power)
// bundle the GUI offers in the Settings → MeshCore → Radio dropdown.
// Operators usually start from the regional preset that matches their
// regulator (US 915, EU 868, AU 915, JP 920) and tweak from there if
// they're operating in a sub-band or against a non-default repeater
// configuration.
//
// FreqHz / BwHz are integers because that's what CmdSetRadioParams
// expects on the wire — the GUI converts to MHz / kHz for display.
//
// SF and CR follow LoRa convention:
//   - SF 7 = 128 chips/symbol (fastest, shortest range)
//   - SF 12 = 4096 chips/symbol (slowest, longest range)
//   - CR 5 = 4/5 redundancy (least overhead)
//   - CR 8 = 4/8 redundancy (most overhead, most robust)
//
// MeshCore's reference defaults across regions are SF 11 + CR 5 at
// 250 kHz BW — a balance between range and packet airtime. Power
// caps reflect each region's regulatory ceiling for amateur /
// licence-exempt LoRa operation.
type RadioPreset struct {
	Name    string
	FreqHz  uint32
	BwHz    uint32
	SF      uint8
	CR      uint8
	TxPower uint8 // dBm
	Note    string
}

// Presets is the canonical preset list. Order: regional regulator
// defaults first (US 915 / EU 868 / AU 915 / JP 920), then known
// MeshCore community subnet configs (regional repeater networks
// pick a non-default freq/SF combo to carve out their own
// channel). Operators outside any of these can leave the dropdown
// at "Custom" and dial values in directly.
var Presets = []RadioPreset{
	{
		Name:    "US 915 MHz",
		FreqHz:  915000000,
		BwHz:    250000,
		SF:      11,
		CR:      5,
		TxPower: 22,
		Note:    "FCC Part 15 ISM. 22 dBm cap is the FCC limit for spread-spectrum at +30 dBm EIRP with a typical antenna.",
	},
	{
		Name:    "EU 868 MHz",
		FreqHz:  869525000,
		BwHz:    250000,
		SF:      11,
		CR:      5,
		TxPower: 14,
		Note:    "ETSI EN 300 220 sub-band. 14 dBm cap is the EIRP ceiling for the 868 MHz licence-exempt band.",
	},
	{
		Name:    "AU 915 MHz",
		FreqHz:  915000000,
		BwHz:    250000,
		SF:      11,
		CR:      5,
		TxPower: 20,
		Note:    "ACMA LIPD class licence. 20 dBm conducted power.",
	},
	{
		Name:    "JP 920 MHz",
		FreqHz:  921000000,
		BwHz:    125000,
		SF:      11,
		CR:      5,
		TxPower: 13,
		Note:    "ARIB STD-T108 sub-GHz. 125 kHz BW is the maximum allowed; 13 dBm conducted.",
	},
	// US regional MeshCore community subnets. Narrow 62.5 kHz BW +
	// SF 7-9 trades range for higher per-message data rate vs the
	// default 250 kHz / SF 11; lets the busier metro meshes carry
	// more traffic without choking on slow flood retransmissions.
	// TX power left at the FCC 22 dBm cap; operators with low-gain
	// antennas can dial down per local repeater operator guidance.
	{
		Name:    "SoCal",
		FreqHz:  927875000,
		BwHz:    62500,
		SF:      7,
		CR:      5,
		TxPower: 22,
		Note:    "Southern California regional MeshCore subnet (Los Angeles / OC / SD repeater chain).",
	},
	{
		Name:    "San Francisco",
		FreqHz:  910525000,
		BwHz:    62500,
		SF:      7,
		CR:      5,
		TxPower: 22,
		Note:    "San Francisco Bay Area MeshCore subnet.",
	},
	{
		Name:    "Sacramento",
		FreqHz:  909875000,
		BwHz:    62500,
		SF:      9,
		CR:      5,
		TxPower: 22,
		Note:    "Sacramento Valley MeshCore subnet. SF 9 trades a little throughput for extra range over SoCal/SF defaults.",
	},
	{
		Name:    "Pacific No. West",
		FreqHz:  910525000,
		BwHz:    62500,
		SF:      7,
		CR:      5,
		TxPower: 22,
		Note:    "Pacific Northwest (Oregon / Washington) MeshCore subnet.",
	},
}

// PresetByName returns a preset by case-sensitive name match. The
// second return is false when no preset matches — caller treats
// that as "Custom" (user-supplied values).
func PresetByName(name string) (RadioPreset, bool) {
	for _, p := range Presets {
		if p.Name == name {
			return p, true
		}
	}
	return RadioPreset{}, false
}
