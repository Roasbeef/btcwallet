package wallet

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/psbt"
)

// PsbtFundReq...
type PsbtFundReq struct {
	// Outputs...
	Outputs []*wire.TxOut

	// Account...
	Account uint32

	// MinConf...
	MinConf uint32

	// FeeSatPerKb..
	FeeSatPerKb btcutil.Amount
}

// FundPsbt...
func (w *Wallet) FundPsbtPacket(fundReq *PsbtFundReq) (*psbt.Packet, error) {

	// TODO(roasbeef): actually can't use diretly? instead need to manually
	// perform coin selection??
	simpleTx, err := w.CreateSimpleTx(
		fundReq.Account, fundReq.Outputs, fundReq.MinConf, 
		fundReq.FeeSatPerKb, false,
	)
	if err != nil {
		return nil, err
	}

	fundedPsbt, err := psbt.NewFromUnsignedTx(simpleTx.Tx)
	if err != nil {
	    return nil, err
	}

	for -, txIn := range simpleTx.Tx.TxIn {
	    err := fundedPsbt.AddInSighashType(txscript.SigHashAll, i)
	    if err != nil {
		return nil, err
	    }

	    // if it's a nested p2wh, then need to add redeem script of the
	    // witness program
	    // AddInRedeemScript()

	    // mirrior FetchInputInfo in lnd
	    err := fundedPsbt.AddInWitnessUtxo()
	    if err != nil {
		return nil, err
	    }

	    // examine the inputs added in the authored tx and add to psbt
	    //  * for non-witness
	    //  * for p2wkh
	    //  * for np2wkh

	}

	// may also need to add change addresses

	return nil, nil
}

//
func (w *Wallet) SignPsbtPacket(txPacket *psbt.Packet) (bool, error) {
	// for each input look at data to see if ours

	// if so generate sig

	// then use updater to add all relevant information

	// bool if the tx is fully complete, can vm execute if needed
	return false, nil
}
