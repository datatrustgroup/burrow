// Copyright 2017 Monax Industries Limited
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package execution

import (
	"bytes"
	"fmt"
	"sync"

	acm "github.com/hyperledger/burrow/account"
	"github.com/hyperledger/burrow/blockchain"
	"github.com/hyperledger/burrow/event"
	"github.com/hyperledger/burrow/execution/evm"
	"github.com/hyperledger/burrow/logging"
	logging_types "github.com/hyperledger/burrow/logging/types"
	"github.com/hyperledger/burrow/permission"
	ptypes "github.com/hyperledger/burrow/permission/types"
	"github.com/hyperledger/burrow/txs"
	"github.com/hyperledger/burrow/word"
	"github.com/tendermint/go-crypto"
	"github.com/tendermint/tmlibs/events"
)

type BatchQuerier interface {
	acm.GetterAndStorageGetter
}

type BatchExecutor interface {
	acm.Updater
	acm.Storage
	// Execute transaction against block cache (i.e. block buffer)
	Execute(tx txs.Tx) error
	// Reset executor to underlying State
	Reset() error
}

// Executes transactions
type BatchCommitter interface {
	BatchExecutor
	// Commit execution results to underlying State and provide opportunity
	// to mutate state before it is saved
	Commit() (stateHash []byte, err error)
}

type executor struct {
	mtx sync.Mutex
	// cache so we are not copying GenesisDoc repeatedly
	chainID    string
	runCall    bool
	state      *State
	blockchain blockchain.BlockchainTip
	blockCache *BlockCache
	fireable   events.Fireable
	eventCache *events.EventCache
	logger     logging_types.InfoTraceLogger
}

var _ BatchExecutor = (*executor)(nil)

func NewBatchChecker(state *State, blockchain blockchain.BlockchainTip, logger logging_types.InfoTraceLogger) BatchExecutor {
	return newExecutor(false, state, blockchain, event.NewNoOpFireable(),
		logging.WithScope(logger, "NewBatchExecutor"))
}

func NewBatchCommitter(state *State, blockchain blockchain.BlockchainTip,
	fireable events.Fireable,
	logger logging_types.InfoTraceLogger) BatchCommitter {
	return newExecutor(true, state, blockchain, fireable,
		logging.WithScope(logger, "NewBatchCommitter"))
}

func newExecutor(runCall bool,
	state *State,
	blockchain blockchain.BlockchainTip,
	eventFireable events.Fireable,
	logger logging_types.InfoTraceLogger) *executor {
	return &executor{
		chainID:    blockchain.ChainID(),
		runCall:    runCall,
		state:      state,
		blockchain: blockchain,
		blockCache: NewBlockCache(state),
		fireable:   eventFireable,
		eventCache: events.NewEventCache(eventFireable),
		logger:     logger,
	}
}
func (exe *executor) GetAccount(address acm.Address) *acm.ConcreteAccount {
	return exe.blockCache.GetAccount(address)
}

func (exe *executor) UpdateAccount(account *acm.ConcreteAccount) {
	exe.blockCache.UpdateAccount(account)
}

func (exe *executor) RemoveAccount(address acm.Address) {
	exe.blockCache.RemoveAccount(address)
}

func (exe *executor) GetStorage(address acm.Address, key word.Word256) word.Word256 {
	return exe.blockCache.GetStorage(address, key)
}

func (exe *executor) SetStorage(address acm.Address, key word.Word256, value word.Word256) {
	exe.blockCache.SetStorage(address, key, value)
}

func (exe *executor) Commit() ([]byte, error) {
	exe.mtx.Lock()
	defer exe.mtx.Unlock()
	// sync the cache
	exe.blockCache.Sync()
	// flush events to listeners (XXX: note issue with blocking)
	exe.eventCache.Flush()
	// save state to disk
	exe.state.Save()
	return exe.state.Hash(), nil
}

func (exe *executor) Reset() error {
	exe.blockCache = NewBlockCache(exe.state)
	exe.eventCache = events.NewEventCache(exe.fireable)
	return nil
}

// If the tx is invalid, an error will be returned.
// Unlike ExecBlock(), state will not be altered.
func (exe *executor) Execute(tx txs.Tx) (err error) {
	exe.logger = logging.WithScope(exe.logger, "executor.Execute(tx txs.Tx)")
	// TODO: do something with fees
	fees := int64(0)

	// Exec tx
	switch tx := tx.(type) {
	case *txs.SendTx:
		accounts, err := getInputs(exe.blockCache.GetAccount, tx.Inputs)
		if err != nil {
			return err
		}

		// ensure all inputs have send permissions
		if !hasSendPermission(exe.blockCache.GetAccount, accounts, exe.logger) {
			return fmt.Errorf("at least one input lacks permission for SendTx")
		}

		// add outputs to accounts map
		// if any outputs don't exist, all inputs must have CreateAccount perm
		accounts, err = getOrMakeOutputs(exe.blockCache.GetAccount, accounts, tx.Outputs, exe.logger)
		if err != nil {
			return err
		}

		signBytes := acm.SignBytes(exe.chainID, tx)
		inTotal, err := validateInputs(accounts, signBytes, tx.Inputs)
		if err != nil {
			return err
		}
		outTotal, err := validateOutputs(tx.Outputs)
		if err != nil {
			return err
		}
		if outTotal > inTotal {
			return txs.ErrTxInsufficientFunds
		}
		fee := inTotal - outTotal
		fees += fee

		// Good! Adjust accounts
		adjustByInputs(accounts, tx.Inputs)
		adjustByOutputs(accounts, tx.Outputs)
		for _, acc := range accounts {
			exe.blockCache.UpdateAccount(acc)
		}

		// if the exe.eventCache is nil, nothing will happen
		if exe.eventCache != nil {
			for _, i := range tx.Inputs {
				exe.eventCache.FireEvent(evm.EventStringAccInput(i.Address), evm.EventDataTx{tx, nil, ""})
			}

			for _, o := range tx.Outputs {
				exe.eventCache.FireEvent(evm.EventStringAccOutput(o.Address), evm.EventDataTx{tx, nil, ""})
			}
		}
		return nil

	case *txs.CallTx:
		var inAcc, outAcc *acm.ConcreteAccount

		// Validate input
		inAcc = exe.blockCache.GetAccount(tx.Input.Address)
		if inAcc == nil {
			logging.InfoMsg(exe.logger, "Cannot find input account",
				"tx_input", tx.Input)
			return txs.ErrTxInvalidAddress
		}

		createContract := tx.Address == acm.ZeroAddress
		if createContract {
			if !hasCreateContractPermission(exe.blockCache.GetAccount, inAcc, exe.logger) {
				return fmt.Errorf("account %s does not have CreateContract permission", tx.Input.Address)
			}
		} else {
			if !hasCallPermission(exe.blockCache.GetAccount, inAcc, exe.logger) {
				return fmt.Errorf("account %s does not have Call permission", tx.Input.Address)
			}
		}

		// pubKey should be present in either "inAcc" or "tx.Input"
		if err := checkInputPubKey(inAcc, tx.Input); err != nil {
			logging.InfoMsg(exe.logger, "Cannot find public key for input account",
				"tx_input", tx.Input)
			return err
		}
		signBytes := acm.SignBytes(exe.chainID, tx)
		err := validateInput(inAcc, signBytes, tx.Input)
		if err != nil {
			logging.InfoMsg(exe.logger, "validateInput failed",
				"tx_input", tx.Input, "error", err)
			return err
		}
		if tx.Input.Amount < tx.Fee {
			logging.InfoMsg(exe.logger, "Sender did not send enough to cover the fee",
				"tx_input", tx.Input)
			return txs.ErrTxInsufficientFunds
		}

		if !createContract {
			// check if its a native contract
			if evm.RegisteredNativeContract(tx.Address.Word256()) {
				return fmt.Errorf("attempt to call a native contract at %s, "+
					"but native contracts cannot be called using CallTx. Use a "+
					"contract that calls the native contract or the appropriate tx "+
					"type (eg. PermissionsTx, NameTx)", tx.Address)
			}

			// Output account may be nil if we are still in mempool and contract was created in same block as this tx
			// but that's fine, because the account will be created properly when the create tx runs in the block
			// and then this won't return nil. otherwise, we take their fee
			outAcc = exe.blockCache.GetAccount(tx.Address)
		}

		exe.logger.Trace("output_account", outAcc)

		// Good!
		value := tx.Input.Amount - tx.Fee

		inAcc.Sequence += 1
		inAcc.Balance -= tx.Fee
		exe.blockCache.UpdateAccount(inAcc)

		// The logic in runCall MUST NOT return.
		if exe.runCall {

			// VM call variables
			var (
				gas     int64                = tx.GasLimit
				err     error                = nil
				caller  *acm.ConcreteAccount = inAcc.Copy()
				callee  *acm.ConcreteAccount = nil // initialized below
				code    []byte               = nil
				ret     []byte               = nil
				txCache                      = NewTxCache(exe.blockCache)
				params                       = evm.Params{
					BlockHeight: int64(exe.blockchain.LastBlockHeight()),
					BlockHash:   word.LeftPadWord256(exe.blockchain.LastBlockHash()),
					BlockTime:   exe.blockchain.LastBlockTime().Unix(),
					GasLimit:    GasLimit,
				}
			)

			if !createContract && (outAcc == nil || len(outAcc.Code) == 0) {
				// if you call an account that doesn't exist
				// or an account with no code then we take fees (sorry pal)
				// NOTE: it's fine to create a contract and call it within one
				// block (nonce will prevent re-ordering of those txs)
				// but to create with one contract and call with another
				// you have to wait a block to avoid a re-ordering attack
				// that will take your fees
				if outAcc == nil {
					logging.InfoMsg(exe.logger, "Call to address that does not exist",
						"caller_address", inAcc.Address,
						"callee_address", tx.Address)
				} else {
					logging.InfoMsg(exe.logger, "Call to address that holds no code",
						"caller_address", inAcc.Address,
						"callee_address", tx.Address)
				}
				err = txs.ErrTxInvalidAddress
				goto CALL_COMPLETE
			}

			// get or create callee
			if createContract {
				// We already checked for permission
				callee = txCache.CreateAccount(caller)
				logging.TraceMsg(exe.logger, "Created new contract",
					"contract_address", callee.Address,
					"contract_code", callee.Code)
				code = tx.Data
			} else {
				callee = outAcc.Copy()
				logging.TraceMsg(exe.logger, "Calling existing contract",
					"contract_address", callee.Address,
					"contract_code", callee.Code)
				code = callee.Code
			}
			exe.logger.Trace("callee_")

			// Run VM call and sync txCache to exe.blockCache.
			{ // Capture scope for goto.
				// Write caller/callee to txCache.
				txCache.UpdateAccount(caller)
				txCache.UpdateAccount(callee)
				vmach := evm.NewVM(txCache, evm.DefaultDynamicMemoryProvider, params,
					caller.Address.Word256(), txs.TxHash(exe.chainID, tx))
				vmach.SetFireable(exe.eventCache)
				// NOTE: Call() transfers the value from caller to callee iff call succeeds.
				ret, err = vmach.Call(caller, callee, code, tx.Data, value, &gas)
				if err != nil {
					// Failure. Charge the gas fee. The 'value' was otherwise not transferred.
					logging.InfoMsg(exe.logger, "Error on execution",
						"error", err)
					goto CALL_COMPLETE
				}

				logging.TraceMsg(exe.logger, "Successful execution")
				if createContract {
					callee.Code = ret
				}
				txCache.Sync(exe.blockCache)
			}

		CALL_COMPLETE: // err may or may not be nil.

			// Create a receipt from the ret and whether it erred.
			logging.TraceMsg(exe.logger, "VM call complete",
				"caller", caller,
				"callee", callee,
				"return", ret,
				"error", err)

			// Fire Events for sender and receiver
			// a separate event will be fired from vm for each additional call
			if exe.eventCache != nil {
				exception := ""
				if err != nil {
					exception = err.Error()
				}
				exe.eventCache.FireEvent(evm.EventStringAccInput(tx.Input.Address), evm.EventDataTx{tx, ret, exception})
				exe.eventCache.FireEvent(evm.EventStringAccOutput(tx.Address), evm.EventDataTx{tx, ret, exception})
			}
		} else {
			// The mempool does not call txs until
			// the proposer determines the order of txs.
			// So mempool will skip the actual .Call(),
			// and only deduct from the caller's balance.
			inAcc.Balance -= value
			if createContract {
				inAcc.Sequence += 1 // XXX ?!
			}
			exe.blockCache.UpdateAccount(inAcc)
		}

		return nil

	case *txs.NameTx:
		var inAcc *acm.ConcreteAccount

		// Validate input
		inAcc = exe.blockCache.GetAccount(tx.Input.Address)
		if inAcc == nil {
			logging.InfoMsg(exe.logger, "Cannot find input account",
				"tx_input", tx.Input)
			return txs.ErrTxInvalidAddress
		}
		// check permission
		if !hasNamePermission(exe.blockCache.GetAccount, inAcc, exe.logger) {
			return fmt.Errorf("account %s does not have Name permission", tx.Input.Address)
		}
		// pubKey should be present in either "inAcc" or "tx.Input"
		if err := checkInputPubKey(inAcc, tx.Input); err != nil {
			logging.InfoMsg(exe.logger, "Cannot find public key for input account",
				"tx_input", tx.Input)
			return err
		}
		signBytes := acm.SignBytes(exe.chainID, tx)
		err := validateInput(inAcc, signBytes, tx.Input)
		if err != nil {
			logging.InfoMsg(exe.logger, "validateInput failed",
				"tx_input", tx.Input, "error", err)
			return err
		}
		if tx.Input.Amount < tx.Fee {
			logging.InfoMsg(exe.logger, "Sender did not send enough to cover the fee",
				"tx_input", tx.Input)
			return txs.ErrTxInsufficientFunds
		}

		// validate the input strings
		if err := tx.ValidateStrings(); err != nil {
			return err
		}

		value := tx.Input.Amount - tx.Fee

		// let's say cost of a name for one block is len(data) + 32
		costPerBlock := txs.NameCostPerBlock(txs.NameBaseCost(tx.Name, tx.Data))
		expiresIn := uint64(value / costPerBlock)
		lastBlockHeight := exe.blockchain.LastBlockHeight()

		logging.TraceMsg(exe.logger, "New NameTx",
			"value", value,
			"cost_per_block", costPerBlock,
			"expires_in", expiresIn,
			"last_block_height", lastBlockHeight)

		// check if the name exists
		entry := exe.blockCache.GetNameRegEntry(tx.Name)

		if entry != nil {
			var expired bool

			// if the entry already exists, and hasn't expired, we must be owner
			if entry.Expires > lastBlockHeight {
				// ensure we are owner
				if entry.Owner != tx.Input.Address {
					logging.InfoMsg(exe.logger, "Sender is trying to update a name for which they are not an owner",
						"sender_address", tx.Input.Address,
						"name", tx.Name)
					return txs.ErrTxPermissionDenied
				}
			} else {
				expired = true
			}

			// no value and empty data means delete the entry
			if value == 0 && len(tx.Data) == 0 {
				// maybe we reward you for telling us we can delete this crap
				// (owners if not expired, anyone if expired)
				logging.TraceMsg(exe.logger, "Removing NameReg entry (no value and empty data in tx requests this)",
					"name", entry.Name)
				exe.blockCache.RemoveNameRegEntry(entry.Name)
			} else {
				// update the entry by bumping the expiry
				// and changing the data
				if expired {
					if expiresIn < txs.MinNameRegistrationPeriod {
						return fmt.Errorf("Names must be registered for at least %d blocks", txs.MinNameRegistrationPeriod)
					}
					entry.Expires = lastBlockHeight + expiresIn
					entry.Owner = tx.Input.Address
					logging.TraceMsg(exe.logger, "An old NameReg entry has expired and been reclaimed",
						"name", entry.Name,
						"expires_in", expiresIn,
						"owner", entry.Owner)
				} else {
					// since the size of the data may have changed
					// we use the total amount of "credit"
					oldCredit := int64(entry.Expires-lastBlockHeight) * txs.NameBaseCost(entry.Name, entry.Data)
					credit := oldCredit + value
					expiresIn = uint64(credit / costPerBlock)
					if expiresIn < txs.MinNameRegistrationPeriod {
						return fmt.Errorf("names must be registered for at least %d blocks", txs.MinNameRegistrationPeriod)
					}
					entry.Expires = lastBlockHeight + expiresIn
					logging.TraceMsg(exe.logger, "Updated NameReg entry",
						"name", entry.Name,
						"expires_in", expiresIn,
						"old_credit", oldCredit,
						"value", value,
						"credit", credit)
				}
				entry.Data = tx.Data
				exe.blockCache.UpdateNameRegEntry(entry)
			}
		} else {
			if expiresIn < txs.MinNameRegistrationPeriod {
				return fmt.Errorf("Names must be registered for at least %d blocks", txs.MinNameRegistrationPeriod)
			}
			// entry does not exist, so create it
			entry = &NameRegEntry{
				Name:    tx.Name,
				Owner:   tx.Input.Address,
				Data:    tx.Data,
				Expires: lastBlockHeight + expiresIn,
			}
			logging.TraceMsg(exe.logger, "Creating NameReg entry",
				"name", entry.Name,
				"expires_in", expiresIn)
			exe.blockCache.UpdateNameRegEntry(entry)
		}

		// TODO: something with the value sent?

		// Good!
		inAcc.Sequence += 1
		inAcc.Balance -= value
		exe.blockCache.UpdateAccount(inAcc)

		// TODO: maybe we want to take funds on error and allow txs in that don't do anythingi?

		if exe.eventCache != nil {
			exe.eventCache.FireEvent(evm.EventStringAccInput(tx.Input.Address), evm.EventDataTx{tx, nil, ""})
			exe.eventCache.FireEvent(evm.EventStringNameReg(tx.Name), evm.EventDataTx{tx, nil, ""})
		}

		return nil

		// Consensus related Txs inactivated for now
		// TODO!
		/*
			case *txs.BondTx:
						valInfo := exe.blockCache.State().GetValidatorInfo(tx.PubKey.Address())
						if valInfo != nil {
							// TODO: In the future, check that the validator wasn't destroyed,
							// add funds, merge UnbondTo outputs, and unbond validator.
							return errors.New("Adding coins to existing validators not yet supported")
						}

						accounts, err := getInputs(exe.blockCache, tx.Inputs)
						if err != nil {
							return err
						}

						// add outputs to accounts map
						// if any outputs don't exist, all inputs must have CreateAccount perm
						// though outputs aren't created until unbonding/release time
						canCreate := hasCreateAccountPermission(exe.blockCache, accounts)
						for _, out := range tx.UnbondTo {
							acc := exe.blockCache.GetAccount(out.Address)
							if acc == nil && !canCreate {
								return fmt.Errorf("At least one input does not have permission to create accounts")
							}
						}

						bondAcc := exe.blockCache.GetAccount(tx.PubKey.Address())
						if !hasBondPermission(exe.blockCache, bondAcc) {
							return fmt.Errorf("The bonder does not have permission to bond")
						}

						if !hasBondOrSendPermission(exe.blockCache, accounts) {
							return fmt.Errorf("At least one input lacks permission to bond")
						}

						signBytes := acm.SignBytes(exe.chainID, tx)
						inTotal, err := validateInputs(accounts, signBytes, tx.Inputs)
						if err != nil {
							return err
						}
						if !tx.PubKey.VerifyBytes(signBytes, tx.Signature) {
							return txs.ErrTxInvalidSignature
						}
						outTotal, err := validateOutputs(tx.UnbondTo)
						if err != nil {
							return err
						}
						if outTotal > inTotal {
							return txs.ErrTxInsufficientFunds
						}
						fee := inTotal - outTotal
						fees += fee

						// Good! Adjust accounts
						adjustByInputs(accounts, tx.Inputs)
						for _, acc := range accounts {
							exe.blockCache.UpdateAccount(acc)
						}
						// Add ValidatorInfo
						_s.SetValidatorInfo(&txs.ValidatorInfo{
							Address:         tx.PubKey.Address(),
							PubKey:          tx.PubKey,
							UnbondTo:        tx.UnbondTo,
							FirstBondHeight: _s.lastBlockHeight + 1,
							FirstBondAmount: outTotal,
						})
						// Add Validator
						added := _s.BondedValidators.Add(&txs.Validator{
							Address:     tx.PubKey.Address(),
							PubKey:      tx.PubKey,
							BondHeight:  _s.lastBlockHeight + 1,
							VotingPower: outTotal,
							Accum:       0,
						})
						if !added {
							PanicCrisis("Failed to add validator")
						}
						if exe.eventCache != nil {
							// TODO: fire for all inputs
							exe.eventCache.FireEvent(txs.EventStringBond(), txs.EventDataTx{tx, nil, ""})
						}
						return nil

					case *txs.UnbondTx:
						// The validator must be active
						_, val := _s.BondedValidators.GetByAddress(tx.Address)
						if val == nil {
							return txs.ErrTxInvalidAddress
						}

						// Verify the signature
						signBytes := acm.SignBytes(exe.chainID, tx)
						if !val.PubKey.VerifyBytes(signBytes, tx.Signature) {
							return txs.ErrTxInvalidSignature
						}

						// tx.Height must be greater than val.LastCommitHeight
						if tx.Height <= val.LastCommitHeight {
							return errors.New("Invalid unbond height")
						}

						// Good!
						_s.unbondValidator(val)
						if exe.eventCache != nil {
							exe.eventCache.FireEvent(txs.EventStringUnbond(), txs.EventDataTx{tx, nil, ""})
						}
						return nil

					case *txs.RebondTx:
						// The validator must be inactive
						_, val := _s.UnbondingValidators.GetByAddress(tx.Address)
						if val == nil {
							return txs.ErrTxInvalidAddress
						}

						// Verify the signature
						signBytes := acm.SignBytes(exe.chainID, tx)
						if !val.PubKey.VerifyBytes(signBytes, tx.Signature) {
							return txs.ErrTxInvalidSignature
						}

						// tx.Height must be in a suitable range
						minRebondHeight := _s.lastBlockHeight - (validatorTimeoutBlocks / 2)
						maxRebondHeight := _s.lastBlockHeight + 2
						if !((minRebondHeight <= tx.Height) && (tx.Height <= maxRebondHeight)) {
							return errors.New(Fmt("Rebond height not in range.  Expected %v <= %v <= %v",
								minRebondHeight, tx.Height, maxRebondHeight))
						}

						// Good!
						_s.rebondValidator(val)
						if exe.eventCache != nil {
							exe.eventCache.FireEvent(txs.EventStringRebond(), txs.EventDataTx{tx, nil, ""})
						}
						return nil

					case *txs.DupeoutTx:
						// Verify the signatures
						_, accused := _s.BondedValidators.GetByAddress(tx.Address)
						if accused == nil {
							_, accused = _s.UnbondingValidators.GetByAddress(tx.Address)
							if accused == nil {
								return txs.ErrTxInvalidAddress
							}
						}
						voteASignBytes := acm.SignBytes(exe.chainID, &tx.VoteA)
						voteBSignBytes := acm.SignBytes(exe.chainID, &tx.VoteB)
						if !accused.PubKey.VerifyBytes(voteASignBytes, tx.VoteA.Signature) ||
							!accused.PubKey.VerifyBytes(voteBSignBytes, tx.VoteB.Signature) {
							return txs.ErrTxInvalidSignature
						}

						// Verify equivocation
						// TODO: in the future, just require one vote from a previous height that
						// doesn't exist on this chain.
						if tx.VoteA.Height != tx.VoteB.Height {
							return errors.New("DupeoutTx heights don't match")
						}
						if tx.VoteA.Round != tx.VoteB.Round {
							return errors.New("DupeoutTx rounds don't match")
						}
						if tx.VoteA.Type != tx.VoteB.Type {
							return errors.New("DupeoutTx types don't match")
						}
						if bytes.Equal(tx.VoteA.BlockHash, tx.VoteB.BlockHash) {
							return errors.New("DupeoutTx blockhashes shouldn't match")
						}

						// Good! (Bad validator!)
						_s.destroyValidator(accused)
						if exe.eventCache != nil {
							exe.eventCache.FireEvent(txs.EventStringDupeout(), txs.EventDataTx{tx, nil, ""})
						}
						return nil
		*/

	case *txs.PermissionsTx:
		var inAcc *acm.ConcreteAccount

		// Validate input
		inAcc = exe.blockCache.GetAccount(tx.Input.Address)
		if inAcc == nil {
			logging.InfoMsg(exe.logger, "Cannot find input account",
				"tx_input", tx.Input)
			return txs.ErrTxInvalidAddress
		}

		permFlag := tx.PermArgs.PermFlag()
		// check permission
		if !HasPermission(exe.blockCache.GetAccount, inAcc, permFlag, exe.logger) {
			return fmt.Errorf("account %s does not have moderator permission %s (%b)", tx.Input.Address,
				permission.PermFlagToString(permFlag), permFlag)
		}

		// pubKey should be present in either "inAcc" or "tx.Input"
		if err := checkInputPubKey(inAcc, tx.Input); err != nil {
			logging.InfoMsg(exe.logger, "Cannot find public key for input account",
				"tx_input", tx.Input)
			return err
		}
		signBytes := acm.SignBytes(exe.chainID, tx)
		err := validateInput(inAcc, signBytes, tx.Input)
		if err != nil {
			logging.InfoMsg(exe.logger, "validateInput failed",
				"tx_input", tx.Input,
				"error", err)
			return err
		}

		value := tx.Input.Amount

		logging.TraceMsg(exe.logger, "New PermissionsTx",
			"perm_flag", permission.PermFlagToString(permFlag),
			"perm_args", tx.PermArgs)

		var permAcc *acm.ConcreteAccount
		switch args := tx.PermArgs.(type) {
		case *permission.HasBaseArgs:
			// this one doesn't make sense from txs
			return fmt.Errorf("HasBase is for contracts, not humans. Just look at the blockchain")
		case *permission.SetBaseArgs:
			if permAcc = exe.blockCache.GetAccount(args.Address); permAcc == nil {
				return fmt.Errorf("trying to update permissions for unknown account %s", args.Address)
			}
			err = permAcc.Permissions.Base.Set(args.Permission, args.Value)
		case *permission.UnsetBaseArgs:
			if permAcc = exe.blockCache.GetAccount(args.Address); permAcc == nil {
				return fmt.Errorf("trying to update permissions for unknown account %s", args.Address)
			}
			err = permAcc.Permissions.Base.Unset(args.Permission)
		case *permission.SetGlobalArgs:
			if permAcc = exe.blockCache.GetAccount(permission.GlobalPermissionsAddress); permAcc == nil {
				panic("can't find global permissions account")
			}
			err = permAcc.Permissions.Base.Set(args.Permission, args.Value)
		case *permission.HasRoleArgs:
			return fmt.Errorf("HasRole is for contracts, not humans. Just look at the blockchain")
		case *permission.AddRoleArgs:
			if permAcc = exe.blockCache.GetAccount(args.Address); permAcc == nil {
				return fmt.Errorf("trying to update roles for unknown account %s", args.Address)
			}
			if !permAcc.Permissions.AddRole(args.Role) {
				return fmt.Errorf("role (%s) already exists for account %s", args.Role, args.Address)
			}
		case *permission.RmRoleArgs:
			if permAcc = exe.blockCache.GetAccount(args.Address); permAcc == nil {
				return fmt.Errorf("trying to update roles for unknown account %s", args.Address)
			}
			if !permAcc.Permissions.RmRole(args.Role) {
				return fmt.Errorf("role (%s) does not exist for account %s", args.Role, args.Address)
			}
		default:
			panic(fmt.Sprintf("invalid permission function: %s", permission.PermFlagToString(permFlag)))
		}

		// TODO: maybe we want to take funds on error and allow txs in that don't do anythingi?
		if err != nil {
			return err
		}

		// Good!
		inAcc.Sequence += 1
		inAcc.Balance -= value
		exe.blockCache.UpdateAccount(inAcc)
		if permAcc != nil {
			exe.blockCache.UpdateAccount(permAcc)
		}

		if exe.eventCache != nil {
			exe.eventCache.FireEvent(evm.EventStringAccInput(tx.Input.Address), evm.EventDataTx{tx, nil, ""})
			exe.eventCache.FireEvent(evm.EventStringPermissions(permission.PermFlagToString(permFlag)), evm.EventDataTx{tx, nil, ""})
		}

		return nil

	default:
		// binary decoding should not let this happen
		panic("Unknown Tx type")
		return nil
	}
}

// ExecBlock stuff is now taken care of by the consensus engine.
// But we leave here for now for reference when we have to do validator updates

/*

// NOTE: If an error occurs during block execution, state will be left
// at an invalid state.  Copy the state before calling ExecBlock!
func ExecBlock(s *State, block *txs.Block, blockPartsHeader txs.PartSetHeader) error {
	err := execBlock(s, block, blockPartsHeader)
	if err != nil {
		return err
	}
	// State.Hash should match block.StateHash
	stateHash := s.Hash()
	if !bytes.Equal(stateHash, block.StateHash) {
		return errors.New(Fmt("Invalid state hash. Expected %X, got %X",
			stateHash, block.StateHash))
	}
	return nil
}

// executes transactions of a block, does not check block.StateHash
// NOTE: If an error occurs during block execution, state will be left
// at an invalid state.  Copy the state before calling execBlock!
func execBlock(s *State, block *txs.Block, blockPartsHeader txs.PartSetHeader) error {
	// Basic block validation.
	err := block.ValidateBasic(s.chainID, s.lastBlockHeight, s.lastBlockAppHash, s.LastBlockParts, s.lastBlockTime)
	if err != nil {
		return err
	}

	// Validate block LastValidation.
	if block.Height == 1 {
		if len(block.LastValidation.Precommits) != 0 {
			return errors.New("Block at height 1 (first block) should have no LastValidation precommits")
		}
	} else {
		if len(block.LastValidation.Precommits) != s.LastBondedValidators.Size() {
			return errors.New(Fmt("Invalid block validation size. Expected %v, got %v",
				s.LastBondedValidators.Size(), len(block.LastValidation.Precommits)))
		}
		err := s.LastBondedValidators.VerifyValidation(
			s.chainID, s.lastBlockAppHash, s.LastBlockParts, block.Height-1, block.LastValidation)
		if err != nil {
			return err
		}
	}

	// Update Validator.LastCommitHeight as necessary.
	for i, precommit := range block.LastValidation.Precommits {
		if precommit == nil {
			continue
		}
		_, val := s.LastBondedValidators.GetByIndex(i)
		if val == nil {
			PanicCrisis(Fmt("Failed to fetch validator at index %v", i))
		}
		if _, val_ := s.BondedValidators.GetByAddress(val.Address); val_ != nil {
			val_.LastCommitHeight = block.Height - 1
			updated := s.BondedValidators.Update(val_)
			if !updated {
				PanicCrisis("Failed to update bonded validator LastCommitHeight")
			}
		} else if _, val_ := s.UnbondingValidators.GetByAddress(val.Address); val_ != nil {
			val_.LastCommitHeight = block.Height - 1
			updated := s.UnbondingValidators.Update(val_)
			if !updated {
				PanicCrisis("Failed to update unbonding validator LastCommitHeight")
			}
		} else {
			PanicCrisis("Could not find validator")
		}
	}

	// Remember LastBondedValidators
	s.LastBondedValidators = s.BondedValidators.Copy()

	// Create BlockCache to cache changes to state.
	blockCache := NewBlockCache(s)

	// Execute each tx
	for _, tx := range block.Data.Txs {
		err := ExecTx(blockCache, tx, true, s.eventCache)
		if err != nil {
			return InvalidTxError{tx, err}
		}
	}

	// Now sync the BlockCache to the backend.
	blockCache.Sync()

	// If any unbonding periods are over,
	// reward account with bonded coins.
	toRelease := []*txs.Validator{}
	s.UnbondingValidators.Iterate(func(index int, val *txs.Validator) bool {
		if val.UnbondHeight+unbondingPeriodBlocks < block.Height {
			toRelease = append(toRelease, val)
		}
		return false
	})
	for _, val := range toRelease {
		s.releaseValidator(val)
	}

	// If any validators haven't signed in a while,
	// unbond them, they have timed out.
	toTimeout := []*txs.Validator{}
	s.BondedValidators.Iterate(func(index int, val *txs.Validator) bool {
		lastActivityHeight := MaxInt(val.BondHeight, val.LastCommitHeight)
		if lastActivityHeight+validatorTimeoutBlocks < block.Height {
			log.Notice("Validator timeout", "validator", val, "height", block.Height)
			toTimeout = append(toTimeout, val)
		}
		return false
	})
	for _, val := range toTimeout {
		s.unbondValidator(val)
	}

	// Increment validator AccumPowers
	s.BondedValidators.IncrementAccum(1)
	s.lastBlockHeight = block.Height
	s.lastBlockAppHash = block.Hash()
	s.LastBlockParts = blockPartsHeader
	s.lastBlockTime = block.Time
	return nil
}
*/

// The accounts from the TxInputs must either already have
// acm.PubKey.(type) != nil, (it must be known),
// or it must be specified in the TxInput.  If redeclared,
// the TxInput is modified and input.PubKey set to nil.
func getInputs(accountGetter func(address acm.Address) *acm.ConcreteAccount,
	ins []*txs.TxInput) (map[acm.Address]*acm.ConcreteAccount, error) {
	accounts := map[acm.Address]*acm.ConcreteAccount{}
	for _, in := range ins {
		// Account shouldn't be duplicated
		if _, ok := accounts[in.Address]; ok {
			return nil, txs.ErrTxDuplicateAddress
		}
		acc := accountGetter(in.Address)
		if acc == nil {
			return nil, txs.ErrTxInvalidAddress
		}
		// PubKey should be present in either "account" or "in"
		if err := checkInputPubKey(acc, in); err != nil {
			return nil, err
		}
		accounts[in.Address] = acc
	}
	return accounts, nil
}

func getOrMakeOutputs(accountGetter func(address acm.Address) *acm.ConcreteAccount, accs map[acm.Address]*acm.ConcreteAccount,
	outs []*txs.TxOutput, logger logging_types.InfoTraceLogger) (map[acm.Address]*acm.ConcreteAccount, error) {
	if accs == nil {
		accs = make(map[acm.Address]*acm.ConcreteAccount)
	}

	// we should err if an account is being created but the inputs don't have permission
	var checkedCreatePerms bool
	for _, out := range outs {
		// Account shouldn't be duplicated
		if _, ok := accs[out.Address]; ok {
			return nil, txs.ErrTxDuplicateAddress
		}
		acc := accountGetter(out.Address)
		// output account may be nil (new)
		if acc == nil {
			if !checkedCreatePerms {
				if !hasCreateAccountPermission(accountGetter, accs, logger) {
					return nil, fmt.Errorf("At least one input does not have permission to create accounts")
				}
				checkedCreatePerms = true
			}
			acc = &acm.ConcreteAccount{
				Address:     out.Address,
				Sequence:    0,
				Balance:     0,
				Permissions: permission.ZeroAccountPermissions,
			}
		}
		accs[out.Address] = acc
	}
	return accs, nil
}

// Since all ethereum accounts implicitly exist we sometimes lazily create an Account object to represent them
// only when needed. Sometimes we need to create an unknown Account knowing only its address (which is expected to
// be a deterministic hash of its associated public key) and not its public key. When we eventually receive a
// transaction acting on behalf of that account we will be given a public key that we can check matches the address.
// If it does then we will associate the public key with the stub account already registered in the system once and
// for all time.
func checkInputPubKey(acc *acm.ConcreteAccount, in *txs.TxInput) error {
	if acc.PubKey.Unwrap() == nil {
		if in.PubKey.Unwrap() == nil {
			return txs.ErrTxUnknownPubKey
		}
		if !bytes.Equal(in.PubKey.Address(), acc.Address.Bytes()) {
			return txs.ErrTxInvalidPubKey
		}
		acc.PubKey = in.PubKey
	} else {
		in.PubKey = crypto.PubKey{}
	}
	return nil
}

func validateInputs(accs map[acm.Address]*acm.ConcreteAccount, signBytes []byte, ins []*txs.TxInput) (total int64, err error) {
	for _, in := range ins {
		acc := accs[in.Address]
		if acc == nil {
			panic("validateInputs() expects account in accounts")
		}
		err = validateInput(acc, signBytes, in)
		if err != nil {
			return
		}
		// Good. Add amount to total
		total += in.Amount
	}
	return total, nil
}

func validateInput(acc *acm.ConcreteAccount, signBytes []byte, in *txs.TxInput) (err error) {
	// Check TxInput basic
	if err := in.ValidateBasic(); err != nil {
		return err
	}
	// Check signatures
	if !acc.PubKey.VerifyBytes(signBytes, in.Signature) {
		return txs.ErrTxInvalidSignature
	}
	// Check sequences
	if acc.Sequence+1 != in.Sequence {
		return txs.ErrTxInvalidSequence{
			Got:      in.Sequence,
			Expected: acc.Sequence + 1,
		}
	}
	// Check amount
	if acc.Balance < in.Amount {
		return txs.ErrTxInsufficientFunds
	}
	return nil
}

func validateOutputs(outs []*txs.TxOutput) (total int64, err error) {
	for _, out := range outs {
		// Check TxOutput basic
		if err := out.ValidateBasic(); err != nil {
			return 0, err
		}
		// Good. Add amount to total
		total += out.Amount
	}
	return total, nil
}

func adjustByInputs(accs map[acm.Address]*acm.ConcreteAccount, ins []*txs.TxInput) {
	for _, in := range ins {
		acc := accs[in.Address]
		if acc == nil {
			panic("adjustByInputs() expects account in accounts")
		}
		if acc.Balance < in.Amount {
			panic("adjustByInputs() expects sufficient funds")
		}
		acc.Balance -= in.Amount

		acc.Sequence += 1
	}
}

func adjustByOutputs(accs map[acm.Address]*acm.ConcreteAccount, outs []*txs.TxOutput) {
	for _, out := range outs {
		acc := accs[out.Address]
		if acc == nil {
			panic("adjustByOutputs() expects account in accounts")
		}
		acc.Balance += out.Amount
	}
}

//---------------------------------------------------------------

// Get permission on an account or fall back to global value
func HasPermission(accountGetter func(address acm.Address) *acm.ConcreteAccount, acc *acm.ConcreteAccount, perm ptypes.PermFlag,
	logger logging_types.InfoTraceLogger) bool {
	if perm > permission.AllPermFlags {
		panic("Checking an unknown permission in state should never happen")
	}

	//if acc == nil {
	// TODO
	// this needs to fall back to global or do some other specific things
	// eg. a bondAcc may be nil and so can only bond if global bonding is true
	//}
	permString := permission.PermFlagToString(perm)

	v, err := acc.Permissions.Base.Get(perm)
	if _, ok := err.(ptypes.ErrValueNotSet); ok {
		if accountGetter == nil {
			panic("All known global permissions should be set!")
		}
		logging.TraceMsg(logger, "Permission for account is not set. Querying GlobalPermissionsAddres.",
			"perm_flag", permString)
		return HasPermission(nil, accountGetter(permission.GlobalPermissionsAddress), perm, logger)
	} else if v {
		logging.TraceMsg(logger, "Account has permission",
			"account_address", acc.Address,
			"perm_flag", permString)
	} else {
		logging.TraceMsg(logger, "Account does not have permission",
			"account_address", acc.Address,
			"perm_flag", permString)
	}
	return v
}

// TODO: for debug log the failed accounts
func hasSendPermission(accountGetter func(address acm.Address) *acm.ConcreteAccount, accs map[acm.Address]*acm.ConcreteAccount,
	logger logging_types.InfoTraceLogger) bool {
	for _, acc := range accs {
		if !HasPermission(accountGetter, acc, permission.Send, logger) {
			return false
		}
	}
	return true
}

func hasNamePermission(accountGetter func(address acm.Address) *acm.ConcreteAccount, acc *acm.ConcreteAccount,
	logger logging_types.InfoTraceLogger) bool {
	return HasPermission(accountGetter, acc, permission.Name, logger)
}

func hasCallPermission(accountGetter func(address acm.Address) *acm.ConcreteAccount, acc *acm.ConcreteAccount,
	logger logging_types.InfoTraceLogger) bool {
	return HasPermission(accountGetter, acc, permission.Call, logger)
}

func hasCreateContractPermission(accountGetter func(address acm.Address) *acm.ConcreteAccount, acc *acm.ConcreteAccount,
	logger logging_types.InfoTraceLogger) bool {
	return HasPermission(accountGetter, acc, permission.CreateContract, logger)
}

func hasCreateAccountPermission(accountGetter func(address acm.Address) *acm.ConcreteAccount, accs map[acm.Address]*acm.ConcreteAccount,
	logger logging_types.InfoTraceLogger) bool {
	for _, acc := range accs {
		if !HasPermission(accountGetter, acc, permission.CreateAccount, logger) {
			return false
		}
	}
	return true
}

func hasBondPermission(accountGetter func(address acm.Address) *acm.ConcreteAccount, acc *acm.ConcreteAccount,
	logger logging_types.InfoTraceLogger) bool {
	return HasPermission(accountGetter, acc, permission.Bond, logger)
}

func hasBondOrSendPermission(accountGetter func(address acm.Address) *acm.ConcreteAccount, accs map[acm.Address]*acm.ConcreteAccount,
	logger logging_types.InfoTraceLogger) bool {
	for _, acc := range accs {
		if !HasPermission(accountGetter, acc, permission.Bond, logger) {
			if !HasPermission(accountGetter, acc, permission.Send, logger) {
				return false
			}
		}
	}
	return true
}

//-----------------------------------------------------------------------------

type InvalidTxError struct {
	Tx     txs.Tx
	Reason error
}

func (txErr InvalidTxError) Error() string {
	return fmt.Sprintf("Invalid tx: [%v] reason: [%v]", txErr.Tx, txErr.Reason)
}
