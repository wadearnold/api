// Copyright 2018 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

package ach

import (
	"strconv"
)

// BatchCTX holds the BatchHeader and BatchControl and all EntryDetail for CTX Entries.
//
// The Corporate Trade Exchange (CTX) application provides the ability to collect and disburse
// funds and information between companies. Generally it is used by businesses paying one another
// for goods or services. These payments replace checks with an electronic process of debiting and
// crediting invoices between the financial institutions of participating companies.
type BatchCTX struct {
	Batch
}

// NewBatchCTX returns a *BatchCTX
func NewBatchCTX(bh *BatchHeader) *BatchCTX {
	batch := new(BatchCTX)
	batch.SetControl(NewBatchControl())
	batch.SetHeader(bh)
	return batch
}

// Validate checks properties of the ACH batch to ensure they match NACHA guidelines.
// This includes computing checksums, totals, and sequence orderings.
//
// Validate will never modify the batch.
func (batch *BatchCTX) Validate() error {
	// basic verification of the batch before we validate specific rules.
	if err := batch.verify(); err != nil {
		return err
	}

	// Add configuration and type specific validation for this type.
	if batch.Header.StandardEntryClassCode != CTX {
		return batch.Error("StandardEntryClassCode", ErrBatchSECType, CTX)
	}

	for _, entry := range batch.Entries {

		// Trapping this error, as entry.CTXAddendaRecordsField() can not be greater than 9999
		if len(entry.Addenda05) > 9999 {
			return batch.Error("AddendaCount", NewErrBatchAddendaCount(len(entry.Addenda05), 9999))
		}

		// validate CTXAddendaRecord Field is equal to the actual number of Addenda records
		// use 0 value if there is no Addenda records
		addendaRecords, _ := strconv.Atoi(entry.CATXAddendaRecordsField())
		if len(entry.Addenda05) != addendaRecords {
			return batch.Error("AddendaCount", NewErrBatchExpectedAddendaCount(len(entry.Addenda05), addendaRecords))
		}

		switch entry.TransactionCode {
		case CheckingPrenoteCredit, CheckingPrenoteDebit, SavingsPrenoteCredit, SavingsReturnNOCDebit, GLPrenoteCredit,
			GLPrenoteDebit, LoanPrenoteCredit:
			return batch.Error("Addendum", ErrBatchTransactionCodeAddenda, entry.TransactionCode)
		default:
		}
		// Verify the TransactionCode is valid for a ServiceClassCode
		if err := batch.ValidTranCodeForServiceClassCode(entry); err != nil {
			return err
		}
		// Verify Addenda* FieldInclusion based on entry.Category and batchHeader.StandardEntryClassCode
		if err := batch.addendaFieldInclusion(entry); err != nil {
			return err
		}
	}
	return nil
}

// Create will tabulate and assemble an ACH batch into a valid state. This includes
// setting any posting dates, sequence numbers, counts, and sums.
//
// Create implementations are free to modify computable fields in a file and should
// call the Batch's Validate() function at the end of their execution.
func (batch *BatchCTX) Create() error {
	// generates sequence numbers and batch control
	if err := batch.build(); err != nil {
		return err
	}
	// Additional steps specific to batch type
	// ...
	return batch.Validate()
}