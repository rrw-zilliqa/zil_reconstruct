package main

import (
	"container/list"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	//"github.com/Zilliqa/gozilliqa-sdk/v3/account"
	"github.com/Zilliqa/gozilliqa-sdk/v3/core"
	//"github.com/Zilliqa/gozilliqa-sdk/v3/crosschain/polynetwork"
	"github.com/Zilliqa/gozilliqa-sdk/v3/multisig"
	"github.com/Zilliqa/gozilliqa-sdk/v3/provider"
	zilutil "github.com/Zilliqa/gozilliqa-sdk/v3/util"

	"encoding/hex"
)

type Verifier struct {
	NumOfDsGuard int
}

var (
	tool       string
	inFile     string
	outFile    string
	blkNum     int64
	srcBlkNum  int64
	guardNodes int
	apiUrl     string
)

func init() {
	flag.StringVar(&tool, "tool", "", "choose a tool to run")
	flag.StringVar(&outFile, "output_file", "", "Zilliqa sync state output file")
	flag.StringVar(&inFile, "input_file", "", "Zilliqa sync state input file")
	flag.Int64Var(&blkNum, "block_number", -1, "For Zilliqa genesis sync, the block number to scan forward to")
	flag.Int64Var(&srcBlkNum, "data_for_block", -1, "For Zilliqa genesis sync, the block number of the ds committee in infile")
	flag.IntVar(&guardNodes, "guard_nodes", 0, "For Zilliqa genesis sync, the number of guard nodes")
	flag.StringVar(&apiUrl, "api", "", "Zilliqa API url")

	flag.Parse()
}

func main() {
	switch tool {
	case "zil_reconstruct_genesis_header":
		ZilReconstructGenesisHeader(inFile, outFile, srcBlkNum, blkNum, guardNodes, apiUrl)
	}
}

func ZilReconstructGenesisHeader(inFile string, ouFile string, srcTxBlockNum int64, targetTxBlockNum int64, numGuards int, apiUrl string) {
	// Load the consensus from a JSON file, and then roll it forwards using the SDK until  you get to the
	// target block.
	const ZILLIQA_EPOCH_BLOCKS = 100
	type TxBlockAndDsComm struct {
		TxBlock *core.TxBlock
		DsBlock *core.DsBlock
		DsComm  []core.PairOfNode
	}
	type DsCommInput struct {
		DsComm []core.PairOfNode
	}
	fmt.Printf("Reading sync data from %s with %d guard nodes\n", inFile, numGuards)
	raw, err := os.ReadFile(inFile)
	if err != nil {
		panic(fmt.Errorf("Cannot read %s - %s", inFile, err.Error()))
	}
	fmt.Printf("Read %d characters. Processing .. \n", len(raw))
	var dsCommInput DsCommInput

	err = json.Unmarshal(raw, &dsCommInput)
	if err != nil {
		panic(fmt.Errorf("Cannot unmarshal %s - %s", string(raw), err.Error()))
	}

	// OK. We now have the block number and ds committee.
	var dsComm *list.List
	dscValid := make(map[int]bool)
	{
		idx := 0
		dsComm = list.New()
		for _, ds := range dsCommInput.DsComm {
			dsComm.PushBack(ds)
			dscValid[idx] = true
			idx++
		}
	}

	zilSdk := provider.NewProvider(apiUrl)
	// We need the current DS Committee because we need to know the number of DSGuards
	// we assume that the number of DS guards stays the same throughout the roll.

	// Grab the original tx block so we can get the ds block number
	fmt.Printf("Retrieving block data for block %d\n", srcTxBlockNum)
	origTxBlockT, err := zilSdk.GetTxBlockVerbose(strconv.Itoa(int(srcTxBlockNum)))
	if err != nil {
		panic(fmt.Errorf("Cannot retrieve block %d", srcTxBlockNum))
	}

	var curDsBlockNum uint64
	var curTxBlockNum uint64

	fmt.Printf("Parsing .. \n")
	origTxBlock := core.NewTxBlockFromTxBlockT(origTxBlockT)
	curDsBlockNum = origTxBlock.BlockHeader.DSBlockNum
	fmt.Printf("Tx Block %d has DS block %d\n", srcTxBlockNum, curDsBlockNum)
	curTxBlockNum = uint64(srcTxBlockNum)
	if curTxBlockNum >= uint64(targetTxBlockNum) {
		panic(fmt.Errorf("Input data file is for TxBlock %d, which is after target %d - cannot roll backwards", curTxBlockNum, targetTxBlockNum))
	}

	fmt.Printf("Starting at txBlock %d, dsBlock %d and moving to TxBlock %d\n",
		curTxBlockNum, curDsBlockNum, targetTxBlockNum)
	verifier := &Verifier{
		NumOfDsGuard: numGuards,
	}

	var nextDsBlockNum uint64

	dsCommitteeVersion := 0

	// Technically speaking, we could do all this just with ds block numbers - there's no
	// reason to get the tx blocks at all, but we do, just as a check.
	for {
		// Find the next Ds block
		nextTxBlockNum := ((curTxBlockNum / ZILLIQA_EPOCH_BLOCKS) + 1) * ZILLIQA_EPOCH_BLOCKS
		if nextTxBlockNum > uint64(targetTxBlockNum) {
			break
		}
		// Now grab it.
		nextTxBlock, err2 := zilSdk.GetTxBlockVerbose(strconv.Itoa(int(nextTxBlockNum)))
		if err2 != nil {
			panic(fmt.Errorf("Cannot retrieve Tx block %d : %v", nextTxBlockNum, err2))
		}
		nextDsBlockNum, _ = strconv.ParseUint(nextTxBlock.Header.DSBlockNum, 10, 64)
		nextDsBlock, err3 := zilSdk.GetDsBlockVerbose(strconv.Itoa(int(nextDsBlockNum)))
		if err3 != nil {
			panic(fmt.Errorf("Cannot retrieve Ds block %d : %v", nextDsBlockNum, err3))
		}
		nextDsBlockNum, _ := strconv.ParseUint(nextDsBlock.Header.BlockNum, 10, 64)
		fmt.Printf("DS Block at TxBlock %d is %d with DSC version %d\n", nextTxBlockNum, nextDsBlockNum, dsCommitteeVersion)
		if nextDsBlockNum != curDsBlockNum {
			fmt.Printf("... advance\n")
			nextDsBlockDecoded := core.NewDsBlockFromDsBlockT(nextDsBlock)
			fmt.Printf("There are %d to be removed, and %d to be added\n",
				len(nextDsBlockDecoded.BlockHeader.RemoveDSNodePubKeys), len(nextDsBlockDecoded.BlockHeader.PoWDSWinners))

			oldDsComm := list.New()
			var curLink *list.Element
			curLink = dsComm.Front()
			for {
				if curLink == nil {
					break
				}
				oldDsComm.PushBack(curLink.Value)
				curLink = curLink.Next()
			}

			newDsList, err2 := verifier.VerifyDsBlock(nextDsBlock, nextDsBlockDecoded, dsComm, &dscValid)
			if err2 != nil {
				panic(fmt.Errorf("Cannot advance DS block past %d - %v", nextDsBlock, err2))
			}
			curDsBlockNum = nextDsBlockNum
			// Check the correspondence between old and new dscs.
			if oldDsComm.Len() != newDsList.Len() {
				dsCommitteeVersion += 1
			} else {
				var elem1 *list.Element
				var elem2 *list.Element
				elem1 = oldDsComm.Front()
				elem2 = newDsList.Front()
				for {
					if elem1 == nil {
						break
					}
					if elem1.Value.(core.PairOfNode).PubKey != elem2.Value.(core.PairOfNode).PubKey {
						fmt.Printf("Cttee change\n")
						dsCommitteeVersion += 1
						break
					}
					elem1 = elem1.Next()
					elem2 = elem2.Next()
				}
			}
			dsComm = newDsList

			// fmt.Printf("After DS block, new committee is")
			// var elem *list.Element
			// elem = dsComm.Front()
			// for {
			// 	if elem == nil {
			// 		break
			// 	}
			// 	fmt.Printf(" > %s", elem.Value.(core.PairOfNode).PubKey)
			// 	elem = elem.Next()
			// }
		}
		curTxBlockNum = nextTxBlockNum
	}

	// OK. Now fetch the other data we need..
	// Todo we actually already did this ^^^ -use that value instead..
	fmt.Printf("Filling data for block %d\n", targetTxBlockNum)
	targetTxBlockV, err4 := zilSdk.GetTxBlockVerbose(strconv.Itoa(int(targetTxBlockNum)))
	if err4 != nil {
		panic(fmt.Errorf("Cannot obtain block info for tx block %d", targetTxBlockNum))
	}

	txBlock := core.NewTxBlockFromTxBlockT(targetTxBlockV)
	targetDsBlockNum := txBlock.BlockHeader.DSBlockNum
	if nextDsBlockNum != targetDsBlockNum {
		panic(fmt.Errorf("Internal inconsistency! Target Tx Block %d has DS Block %d, but we computed results for DS block %d. Call rrw",
			targetTxBlockNum, targetDsBlockNum, nextDsBlockNum))
	}
	targetDsBlockV, err5 := zilSdk.GetDsBlockVerbose(strconv.Itoa(int(targetDsBlockNum)))
	if err5 != nil {
		panic(fmt.Errorf("Cannot obtain ds block info for ds block %d", targetDsBlockNum))
	}
	dsBlock := core.NewDsBlockFromDsBlockT(targetDsBlockV)
	var dsCommArr []core.PairOfNode
	var elem *list.Element
	elem = dsComm.Front()
	for {
		if elem == nil {
			break
		}
		// DANGER WILL ROBINSON!
		// gozilliqa compares keys by literal string value against the values retrieved from the blockchain
		// values retrieved from the blockchain are upper-case with a leading 0x. If you don't force them
		// to have this format when you save them, they will not compare properly, we will believe all
		// to-be-removed keys are not in the DS committee (because they are upper-case and some of the
		// elements we put in the DSC wil be lower-case) and DSC tracking will fail. Since it is impossible
		// to fix the SDK (it is pinned in eg. ZilBridge), we must ensure here that output keys
		// are in the right format. Ugh! - rrw 2024-01-27
		value := elem.Value.(core.PairOfNode)
		if value.PubKey[:2] != "0x" {
			panic(fmt.Errorf("DS committee member %s does not start with 0x, but %s- see comment above this source position!",
				value.PubKey, value.PubKey[:2]))
		}
		value.PubKey = "0x" + strings.ToUpper(value.PubKey[2:])
		dsCommArr = append(dsCommArr, value)
		elem = elem.Next()
	}
	txBlockAndDsComm := TxBlockAndDsComm{
		TxBlock: txBlock,
		DsBlock: dsBlock,
		DsComm:  dsCommArr,
	}
	raw, err7 := json.Marshal(txBlockAndDsComm)
	if err7 != nil {
		panic(fmt.Errorf("Cannot marshal genesis info - %s", err.Error()))
	}
	err = os.WriteFile(outFile, []byte(raw), 0644)
	if err != nil {
		panic(fmt.Errorf("Cannot write output to %s", outFile))
	}
	fmt.Printf("Genesis block state for tx block %d (DS %d) now hopefully reconstructed to %s. Try sync_zil_genesis_header_from_file\n",
		targetTxBlockNum, targetDsBlockNum, outFile)
}

func (v *Verifier) AggregatedPubKeyFromDsComm(dsComm *list.List, dsBlock *core.DsBlock, dscValid *map[int]bool) ([]byte, error) {
	pubKeys, err := v.generateDsCommArray(dsComm, dsBlock, dscValid)
	if err != nil {
		return nil, err
	}
	aggregatedPubKey, err := multisig.AggregatedPubKey(pubKeys)
	if err != nil {
		return nil, err
	}
	return aggregatedPubKey, nil
}

func (v *Verifier) AggregatedPubKeyFromTxComm(dsComm *list.List, txBlock *core.TxBlock) ([]byte, error) {
	pubKeys, err := v.generateDsCommArray2(dsComm, txBlock)
	if err != nil {
		return nil, err
	}
	aggregatedPubKey, err := multisig.AggregatedPubKey(pubKeys)
	if err != nil {
		return nil, err
	}
	return aggregatedPubKey, nil
}

// abstract this two methods
func (v *Verifier) generateDsCommArray(dsComm *list.List, dsBlock *core.DsBlock, dscValid *map[int]bool) ([][]byte, error) {
	if dsComm.Len() != len(dsBlock.Cosigs.B2) {
		return nil, errors.New(fmt.Sprintf("ds list mismatch - expected %d from cosigs, got %d from current estimate", len(dsBlock.Cosigs.B2), dsComm.Len()))
	}
	bitmap := dsBlock.Cosigs.B2
	fmt.Printf("DSC size %d\n", len(bitmap))
	quorum := len(bitmap) / 3 * 2
	trueCount := 0
	for _, signed := range bitmap {
		if signed {
			trueCount++
		}
	}
	if !(trueCount > quorum) {
		return nil, errors.New("quorum error")
	}
	var commKeys []string
	cursor := dsComm.Front()
	for cursor != nil {
		pair := cursor.Value.(core.PairOfNode)
		cursor = cursor.Next()
		commKeys = append(commKeys, pair.PubKey)
	}

	var pubKeys [][]byte
	for index, key := range commKeys {
		if bitmap[index] {
			pubKeys = append(pubKeys, zilutil.DecodeHex(key))
			(*dscValid)[index] = true
		} else {
			fmt.Printf("^[%d] ", index)
		}
	}
	fmt.Printf("\n")
	return pubKeys, nil
}

func (v *Verifier) generateDsCommArray2(dsComm *list.List, txBlock *core.TxBlock) ([][]byte, error) {
	if dsComm.Len() != len(txBlock.Cosigs.B2) {
		return nil, errors.New("ds list mismatch")
	}
	bitmap := txBlock.Cosigs.B2
	quorum := len(bitmap) / 3 * 2
	trueCount := 0
	for _, signed := range bitmap {
		if signed {
			trueCount++
		}
	}
	if !(trueCount > quorum) {
		return nil, errors.New("quorum error")
	}
	var commKeys []string
	cursor := dsComm.Front()
	for cursor != nil {
		pair := cursor.Value.(core.PairOfNode)
		cursor = cursor.Next()
		commKeys = append(commKeys, pair.PubKey)
	}

	var pubKeys [][]byte
	for index, key := range commKeys {
		if txBlock.Cosigs.B2[index] {
			pubKeys = append(pubKeys, zilutil.DecodeHex(key))
		}
	}
	return pubKeys, nil
}

// 0. verify current ds block
// 2. generate next ds committee
// return new ds comm
func (v *Verifier) VerifyDsBlock(origDsBlock *core.DsBlockT, dsBlock *core.DsBlock, dsComm *list.List, dscValid *map[int]bool) (*list.List, error) {
	newDsComm, err2 := v.UpdateDSCommitteeComposition("", dsComm, origDsBlock, dsBlock, dscValid)
	if err2 != nil {
		return nil, err2
	}
	return newDsComm, nil
}

func (v *Verifier) VerifyTxBlock(txBlock *core.TxBlock, dsComm *list.List) error {
	aggregatedPubKey, err := v.AggregatedPubKeyFromTxComm(dsComm, txBlock)
	if err != nil {
		return err
	}
	r, s := txBlock.GetRandS()
	if !multisig.MultiVerify(aggregatedPubKey, txBlock.Serialize(), r, s) {
		msg := fmt.Sprintf("verify tx block %d error - cannot verify that this dsCommittee is correct", txBlock.BlockHeader.BlockNum)
		return errors.New(msg)
	}
	return nil
}

func (v *Verifier) UpdateDSCommitteeComposition(selfKeyPub string, dsComm *list.List, origDsBlock *core.DsBlockT, dsBlock *core.DsBlock, dscValid *map[int]bool) (*list.List, error) {
	var dummy core.MinerInfoDSComm
	return v.updateDSCommitteeComposition(selfKeyPub, dsComm, origDsBlock, dsBlock, dummy, dscValid)
}

// inner type of dsComm is core.PairOfNode
func (v *Verifier) updateDSCommitteeComposition(selfKeyPub string, dsComm *list.List, origDsBlock *core.DsBlockT,
	dsBlock *core.DsBlock, info core.MinerInfoDSComm, dscValid *map[int]bool) (*list.List, error) {

	fmt.Printf("-- DS Committee --\n")
	{
		var elem *list.Element
		var idx int
		idx = 0
		elem = dsComm.Front()
		for {
			if elem == nil {
				break
			}
			fmt.Printf("[%d] : %s\n", idx, elem.Value.(core.PairOfNode).PubKey)
			idx = idx + 1
			elem = elem.Next()
		}
	}
	for idx, val := range origDsBlock.Header.PoWWinners {
		fmt.Printf("+ [%d] %s\n", idx, val)
	}
	for idx, val := range dsBlock.BlockHeader.RemoveDSNodePubKeys {
		fmt.Printf("- [%d] %s\n", idx, val)
	}
	fmt.Printf("----\n")

	var oldDsComm *list.List
	oldDsComm = list.New()
	{
		var elem *list.Element
		elem = dsComm.Front()
		for {
			if elem == nil {
				break
			}
			oldDsComm.PushBack(elem.Value)
			elem = elem.Next()
		}
	}

	// 0. verify ds block first
	validationOK := false
	useDsc := make(map[int]bool)
	aggregatedPubKey, err := v.AggregatedPubKeyFromDsComm(dsComm, dsBlock, &useDsc)
	if err != nil {
		return nil, err
	}
	headerBytes := dsBlock.Serialize()
	r, s := dsBlock.GetRandS()

	fmt.Printf("headerBytes: %s\n", hex.EncodeToString(headerBytes))
	var setVal bool
	if !multisig.MultiVerify(aggregatedPubKey, headerBytes, r, s) {
		msg := fmt.Sprintf("verify ds block %d error - multisig does not check out for this DS committee", dsBlock.BlockHeader.BlockNum)
		fmt.Println(msg)
		// We failed to verify. One of the vectors must be bad.
		setVal = false
		// return nil, errors.New(msg)
	} else {
		fmt.Printf("+++ verify ds %s block OK\n", origDsBlock.Header.BlockNum)
		setVal = true
		validationOK = true
	}
	for key, _ := range useDsc {
		//fmt.Printf(" => [%d] = %v\n", key, setVal)
		(*dscValid)[key] = setVal
	}

	// 1. get the map of all pow winners from the DS block
	winners := dsBlock.BlockHeader.PoWDSWinners
	numOfWinners := len(dsBlock.BlockHeader.PoWDSWinners)

	// 2. get the array of all non-performant nodes to be removed
	removeDSNodePubkeys := dsBlock.BlockHeader.RemoveDSNodePubKeys

	// 3. shuffle the non-performant nodes to the back
	for _, removed := range removeDSNodePubkeys {
		current := dsComm.Front()
		for current != nil {
			pairOfNode := current.Value.(core.PairOfNode)
			if strings.ToLower(pairOfNode.PubKey) == strings.ToLower(removed) {
				break
			}
			current = current.Next()
		}
		if current != nil {
			dsComm.MoveToBack(current)
			fmt.Printf("DS Committee Member %v moved to back\n", removed)
		} else {
			fmt.Printf("Non-DS Committee member %v in removeDSNodePubkeys - ignoring it\n",
				removed)
		}
	}

	// 4. add new winners
	for _, pubKey := range origDsBlock.Header.PoWWinners {
		peer := winners[pubKey]
		w := core.PairOfNode{
			PubKey: pubKey,
			Peer:   peer,
		}
		// Place the current winner node's information in front of the DS Committee
		count := v.NumOfDsGuard
		cursor := dsComm.Front()
		for count > 0 {
			count--
			cursor = cursor.Next()
		}
		fmt.Printf("Node to add: adding %v", w)
		if cursor == nil {
			// The end of the list!
			dsComm.PushBack(w)
		} else {
			dsComm.InsertBefore(w, cursor)
		}
	}

	// 5. remove one node for every winner, maintaining the size of the DS Committee
	for i := 0; i < numOfWinners; i++ {
		back := dsComm.Back()
		fmt.Printf("Removing back of list for winner %d", i)
		dsComm.Remove(back)
	}

	// Tell us what was updated and set its validation vector to false
	{
		var idx int
		var elem1 *list.Element
		var elem2 *list.Element
		elem1 = oldDsComm.Front()
		elem2 = dsComm.Front()

		var i int
		for i = 0; i < dsComm.Len(); i++ {
			val, ok := (*dscValid)[i]
			if ok && val {
				fmt.Printf("-")
			} else {
				fmt.Printf("^")
			}
		}
		fmt.Printf("\n")
		idx = 0
		for {
			if elem1 == nil {
				break
			}
			p1 := elem1.Value.(core.PairOfNode).PubKey
			p2 := elem2.Value.(core.PairOfNode).PubKey
			if p1 != p2 {
				fmt.Printf("** Idx %d %s -> %s : invalidating\n",
					idx, p1, p2)
				(*dscValid)[idx] = false
			}
			elem1 = elem1.Next()
			elem2 = elem2.Next()
			idx++
		}

		for i = 0; i < dsComm.Len(); i++ {
			val, ok := (*dscValid)[i]
			if ok && val {
				fmt.Printf(".")
			} else {
				fmt.Printf("*")
			}
		}
		fmt.Printf("\n")
		fmt.Printf("Validation %v\n", validationOK)
	}

	return dsComm, nil
}
