// NXP Ultra Secured Digital Host Controller (uSDHC) driver
// https://github.com/f-secure-foundry/tamago
//
// IP: https://www.mobiveil.com/esdhc/
//
// Copyright (c) F-Secure Corporation
// https://foundry.f-secure.com
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package usdhc

import (
	"errors"
	"fmt"
	"time"

	"github.com/f-secure-foundry/tamago/bits"
	"github.com/f-secure-foundry/tamago/internal/reg"
)

// SD registers
const (
	// p101, 4.3.13 Send Interface Condition Command (CMD8), SD-PL-7.10
	CMD8_ARG_VHS           = 8
	CMD8_ARG_CHECK_PATTERN = 0

	VHS_HIGH      = 0b0001
	VHS_LOW       = 0b0010
	CHECK_PATTERN = 0b10101010

	// p59, 4.2.3.1 Initialization Command (ACMD41), SD-PL-7.10
	// p198, 5.1 OCR register, SD-PL-7.10
	SD_OCR_BUSY       = 31
	SD_OCR_HCS        = 30
	SD_OCR_UHSII      = 29
	SD_OCR_XPC        = 28
	SD_OCR_S18R       = 24
	SD_OCR_VDD_HV_MAX = 23
	SD_OCR_VDD_HV_MIN = 15
	SD_OCR_VDD_LV     = 7

	// p120, Table 4-32 : Switch Function Commands (class 10), SD-PL-7.10
	SD_SWITCH_MODE = 31
	// p92, Table 4-11 : Available Functions of CMD6, SD-PL-7.10
	SD_SWITCH_ACCESS_MODE_GROUP = 1
	// p95, 4.3.10.4 Switch Function Status, SD-PL-7.10
	SD_SWITCH_STATUS_LENGTH = 64

	// p89, 4.3.10 Switch Function Command, SD-PL-7.10
	MODE_CHECK         = 0
	MODE_SWITCH        = 1
	ACCESS_MODE_HS     = 0x1
	ACCESS_MODE_SDR50  = 0x2
	ACCESS_MODE_SDR104 = 0x3

	// p201 5.3.1 CSD_STRUCTURE, SD-PL-7.10
	SD_CSD_STRUCTURE = 126 + CSD_RSP_OFF

	// p202 5.3.2 CSD Register (CSD Version 1.0), SD-PL-7.10
	SD_CSD_C_SIZE_MULT_1 = 47 + CSD_RSP_OFF
	SD_CSD_C_SIZE_1      = 62 + CSD_RSP_OFF
	SD_CSD_READ_BL_LEN_1 = 80 + CSD_RSP_OFF

	// p209 5.3.3 CSD Register (CSD Version 2.0), SD-PL-7.10
	SD_CSD_C_SIZE_2      = 48 + CSD_RSP_OFF
	SD_CSD_READ_BL_LEN_2 = 80 + CSD_RSP_OFF

	// p212 5.3.4 CSD Register (CSD Version 3.0), SD-PL-7.10
	SD_CSD_C_SIZE_3      = 48 + CSD_RSP_OFF
	SD_CSD_READ_BL_LEN_3 = 80 + CSD_RSP_OFF

	// p23, 2. System Features, SD-PL-7.10
	HS_MBPS     = 25
	SDR50_MBPS  = 50
	SDR104_MBPS = 75 // instead of 104 due to NXP ERR010450

	// Base clock divided by 2 (Single Data Rate mode)
	SDCLKFS_UHS_SDR50 = 0x01
	// SDR50 frequency: 200 / (1 * 2) == 100 MHz

	// Base clock divided by 2 (Single Data Rate mode)
	SDCLKFS_UHS_SDR104 = 0x01 // instead of 0x00 due to NXP ERR010450
	// SDR104 frequency: 200 / (1 * 2) == 100 MHz
)

// SD constants
const (
	SD_DETECT_TIMEOUT     = 1 * time.Second
	SD_DEFAULT_BLOCK_SIZE = 512
)

func (hw *USDHC) switchSD(mode uint32, group int, val uint32) (status []byte, err error) {
	var arg uint32

	// set `no influence` (0xf) for all function groups
	arg = 0x00ffffff
	// set mode check
	bits.SetN(&arg, SD_SWITCH_MODE, 1, mode)
	// set function group
	bits.SetN(&arg, (group-1)*4, 0xf, val)

	status = make([]byte, SD_SWITCH_STATUS_LENGTH)

	// CMD6 - SWITCH - switch mode of operation
	if err = hw.transfer(6, READ, uint64(arg), 1, SD_SWITCH_STATUS_LENGTH, status); err != nil {
		return
	}

	err = hw.waitState(CURRENT_STATE_TRAN, 500*time.Millisecond)

	return
}

// p350, 35.4.4 SD voltage validation flow chart, IMX6FG
func (hw *USDHC) voltageValidationSD() (sd bool, hc bool) {
	var arg uint32
	var hv bool

	// CMD8 - SEND_IF_COND - read device data
	// p101, 4.3.13 Send Interface Condition Command (CMD8), SD-PL-7.10

	bits.SetN(&arg, CMD8_ARG_VHS, 0b1111, VHS_HIGH)
	bits.SetN(&arg, CMD8_ARG_CHECK_PATTERN, 0xff, CHECK_PATTERN)

	if hw.cmd(8, READ, arg, RSP_48, true, true, false, 0) == nil && hw.rsp(0) == arg {
		// HC/LC HV SD 2.x
		hc = true
		hv = true
	} else {
		arg = VHS_LOW<<CMD8_ARG_VHS | CHECK_PATTERN

		if hw.cmd(8, READ, arg, RSP_48, true, true, false, 0) == nil && hw.rsp(0) == arg {
			// LC SD 1.x
			hc = true
		} else {
			// LC SD 2.x
			hv = true
		}
	}

	// ACMD41 - SD_SEND_OP_COND - read capacity information
	// p59, 4.2.3.1 Initialization Command (ACMD41), SD-PL-7.10
	//
	// The ACMD41 full argument is the OCR, despite the standard
	// confusingly naming OCR only bits 23-08 of it (which instead
	// represents part of OCR register voltage window).
	arg = 0

	if hc {
		// SDHC or SDXC supported
		bits.Set(&arg, SD_OCR_HCS)
		// Maximum Performance
		bits.Set(&arg, SD_OCR_XPC)
		// Switch to 1.8V (only check acceptance for speed detection)
		bits.Set(&arg, SD_OCR_S18R)
	}

	if hv {
		// set HV range
		bits.SetN(&arg, SD_OCR_VDD_HV_MIN, 0x1ff, 0x1ff)
	} else {
		bits.Set(&arg, SD_OCR_VDD_LV)
	}

	start := time.Now()

	for time.Since(start) <= SD_DETECT_TIMEOUT {
		// CMD55 - APP_CMD - next command is application specific
		if hw.cmd(55, READ, 0, RSP_48, true, true, false, 0) != nil {
			return false, false
		}

		// ACMD41 - SD_SEND_OP_COND - send operating conditions
		if err := hw.cmd(41, READ, arg, RSP_48, false, false, false, 0); err != nil {
			return false, false
		}

		rsp := hw.rsp(0)

		if bits.Get(&rsp, SD_OCR_BUSY, 1) == 0 {
			continue
		}

		if bits.Get(&rsp, SD_OCR_HCS, 1) == 1 {
			hc = true
		}

		// Select the fastest mandatory speed mode, supported by this
		// driver, according to the card type.

		if bits.Get(&rsp, SD_OCR_S18R, 1) == 1 {
			// UHS-I
			hw.card.Rate = SDR50_MBPS
		} else if bits.Get(&rsp, SD_OCR_UHSII, 1) == 1 {
			// UHS-II
			hw.card.Rate = SDR50_MBPS
		} else {
			// Non UHS-I
			hw.card.Rate = HS_MBPS
		}

		return true, hc
	}

	return false, false
}

func (hw *USDHC) detectCapabilitiesSD() (err error) {
	// CMD9 - SEND_CSD - read device data
	if err = hw.cmd(9, READ, hw.rca, RSP_136, false, true, false, 0); err != nil {
		return
	}

	ver := hw.rspVal(SD_CSD_STRUCTURE, 0b11)

	switch ver {
	case 0:
		// CSD Version 1.0
		c_size_mult := hw.rspVal(SD_CSD_C_SIZE_MULT_1, 0b111)
		c_size := hw.rspVal(SD_CSD_C_SIZE_1, 0xfff)
		read_bl_len := hw.rspVal(SD_CSD_READ_BL_LEN_1, 0xf)

		// p205, C_SIZE, SD-PL-7.10
		hw.card.BlockSize = 2 << (read_bl_len - 1)
		hw.card.Blocks = int((c_size + 1) * (2 << (c_size_mult + 2)))
	case 1:
		// CSD Version 2.0
		c_size := hw.rspVal(SD_CSD_C_SIZE_2, 0x3fffff)
		read_bl_len := hw.rspVal(SD_CSD_READ_BL_LEN_2, 0xf)

		// p210, C_SIZE, SD-PL-7.10
		hw.card.BlockSize = 2 << (read_bl_len - 1)
		hw.card.Blocks = int(c_size+1) * 1024
	case 2:
		// CSD Version 3.0
		c_size := hw.rspVal(SD_CSD_C_SIZE_2, 0xfffffff)
		read_bl_len := hw.rspVal(SD_CSD_READ_BL_LEN_2, 0xf)

		// p213, C_SIZE, SD-PL-7.10
		hw.card.BlockSize = 2 << (read_bl_len - 1)
		hw.card.Blocks = int(c_size+1) * 1024
	default:
		return fmt.Errorf("unsupported CSD version %d", ver)
	}

	return
}

// p60, 4.2.4 Bus Signal Voltage Switch Sequence, SD-PL-7.10
func (hw *USDHC) voltageSwitchSD() (err error) {
	// CMD11 - VOLTAGE_SWITCH - switch to 1.8V signaling
	if err = hw.cmd(11, READ, 0, RSP_48, true, true, false, 0); err != nil {
		return
	}

	if !reg.WaitFor(1*time.Millisecond, hw.pres_state, PRES_STATE_DLSL, 1, 0) {
		return fmt.Errorf("voltage switch failed, invalid data line")
	}

	hw.setClock(-1, -1)

	// SoC uSDHC IO power voltage selection signal (might be unused)
	reg.Set(hw.vend_spec, VEND_SPEC_VSELECT)

	// board specific low voltage selection/indication function
	if hw.LowVoltage != nil && !hw.LowVoltage() {
		return errors.New("voltage switch failed, not at LV")
	}

	time.Sleep(10 * time.Millisecond)

	hw.setClock(DVS_OP, SDCLKFS_OP)

	if !reg.WaitFor(1*time.Millisecond, hw.pres_state, PRES_STATE_DLSL, 1, 1) {
		return fmt.Errorf("voltage switch failed, invalid data line")
	}

	return
}

// p351, 35.4.5 SD card initialization flow chart, IMX6FG
// p57, 4.2.3 Card Initialization and Identification Process, SD-PL-7.10
func (hw *USDHC) initSD() (err error) {
	var arg uint32
	var bus_width uint32
	var mode uint32
	var clk int

	if hw.LowVoltage == nil {
		hw.card.Rate = HS_MBPS
	} else if hw.card.Rate >= SDR50_MBPS {
		if err = hw.voltageSwitchSD(); err != nil {
			hw.card.Rate = HS_MBPS
		}
	}

	// CMD2 - ALL_SEND_CID - get unique card identification
	if err = hw.cmd(2, READ, arg, RSP_136, false, true, false, 0); err != nil {
		return
	}

	// CMD3 - SEND_RELATIVE_ADDR - get relative card address (RCA)
	if err = hw.cmd(3, READ, arg, RSP_48, true, true, false, 0); err != nil {
		return
	}

	if state := (hw.rsp(0) >> STATUS_CURRENT_STATE) & 0b1111; state != CURRENT_STATE_IDENT {
		return fmt.Errorf("card not in ident state (%d)", state)
	}

	if hw.card.Rate == HS_MBPS {
		hw.setClock(-1, -1)
		hw.setClock(DVS_OP, SDCLKFS_OP)
	}

	// set relative card address
	hw.rca = hw.rsp(0) & (0xffff << RCA_ADDR)

	if err = hw.detectCapabilitiesSD(); err != nil {
		return
	}

	// CMD7 - SELECT/DESELECT CARD - enter transfer state
	if err = hw.cmd(7, READ, hw.rca, RSP_48_CHECK_BUSY, true, true, false, 0); err != nil {
		return
	}

	if err = hw.waitState(CURRENT_STATE_TRAN, 1*time.Millisecond); err != nil {
		return
	}

	// CMD55 - APP_CMD - next command is application specific
	if err = hw.cmd(55, READ, hw.rca, RSP_48, true, true, false, 0); err != nil {
		return
	}

	if ((hw.rsp(0) >> STATUS_APP_CMD) & 1) != 1 {
		return fmt.Errorf("card not expecting application command")
	}

	// p118, Table 4-31, SD-PL-7.10
	switch hw.width {
	case 1:
		bus_width = 0b00
	case 4:
		bus_width = 0b10
	default:
		return errors.New("unsupported SD bus width")
	}

	// ACMD6 - SET_BUS_WIDTH - define the card data bus width
	if err = hw.cmd(6, READ, uint32(bus_width), RSP_48, true, true, false, 0); err != nil {
		return
	}

	if hw.card.Rate >= SDR50_MBPS {
		// Check support bits 415:400 for SDR104 mode,
		// p96, 4.3.10.4 Switch Function Status, SD-PL-7.10.
		if status, _ := hw.switchSD(MODE_CHECK, SD_SWITCH_ACCESS_MODE_GROUP, 0xf); status[13]&ACCESS_MODE_SDR104 != 0 {
			hw.card.Rate = SDR104_MBPS
		}
	}

	switch hw.card.Rate {
	case HS_MBPS:
		mode = ACCESS_MODE_HS
		clk = SDCLKFS_HS_SDR
	case SDR50_MBPS:
		mode = ACCESS_MODE_SDR50
		clk = SDCLKFS_UHS_SDR50
	case SDR104_MBPS:
		mode = ACCESS_MODE_SDR104
		clk = SDCLKFS_UHS_SDR104
	default:
		return
	}

	if _, err = hw.switchSD(MODE_SWITCH, SD_SWITCH_ACCESS_MODE_GROUP, mode); err != nil {
		return
	}

	hw.setClock(-1, -1)
	hw.setClock(DVS_HS, clk)

	hw.card.HS = true

	return
}
