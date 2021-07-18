// Watch blocks and report issues (to terminal and to Discord)
//
// Issues:
// 1. Failed Flashbots (or other 0-gas) transaction
// 2. Bundle out of order by effective-gasprice
// 3. Bundle effective-gasprice is lower than lowest non-fb tx gasprice
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/metachris/flashbots/api"
	"github.com/metachris/flashbots/blockcheck"
	"github.com/metachris/go-ethutils/blockswithtx"
	"github.com/metachris/go-ethutils/utils"
)

var silent bool
var sendErrorsToDiscord bool

// Backlog of new blocks that are not yet present in the mev-blocks API (it has ~5 blocks delay)
var BlockBacklog map[int64]*blockswithtx.BlockWithTxReceipts = make(map[int64]*blockswithtx.BlockWithTxReceipts)

// Counts of errors per miner
type MinerErrorCount struct {
	ErrorCounts blockcheck.ErrorCounts

	MinerHash string
	MinerName string
	Blocks    map[int64]bool
}

var MinerErrors map[string]*MinerErrorCount = make(map[string]*MinerErrorCount)

func main() {
	log.SetOutput(os.Stdout)

	ethUri := flag.String("eth", os.Getenv("ETH_NODE"), "Ethereum node URI")
	// recentBundleOrdersPtr := flag.Bool("recentBundleOrder", false, "check recent bundle orders blocks")
	blockHeightPtr := flag.Int64("block", 0, "specific block to check")
	watchPtr := flag.Bool("watch", false, "watch and process new blocks")
	silentPtr := flag.Bool("silent", false, "don't print info about every block")
	discordPtr := flag.Bool("discord", false, "send errors to Discord")
	flag.Parse()

	silent = *silentPtr

	if *discordPtr {
		if len(os.Getenv("DISCORD_WEBHOOK")) == 0 {
			log.Fatal("No DISCORD_WEBHOOK environment variable found!")
		}
		sendErrorsToDiscord = true
	}

	// Connect to the geth node and start the BlockCheckService
	if *ethUri == "" {
		log.Fatal("Pass a valid eth node with -eth argument or ETH_NODE env var.")
	}

	fmt.Printf("Connecting to %s ...", *ethUri)
	client, err := ethclient.Dial(*ethUri)
	utils.Perror(err)
	fmt.Printf(" ok\n")

	if *blockHeightPtr != 0 {
		// get block with receipts
		block, err := blockswithtx.GetBlockWithTxReceipts(client, *blockHeightPtr)
		utils.Perror(err)

		// check the block
		check, err := blockcheck.CheckBlock(block)
		if err != nil {
			fmt.Println("Check at height error:", err)
		}
		msg := check.Sprint(true, false)
		print(msg)
	}

	if *watchPtr {
		log.Println("Start watching...")
		watch(client)
	}
}

func watch(client *ethclient.Client) {
	headers := make(chan *types.Header)
	sub, err := client.SubscribeNewHead(context.Background(), headers)
	utils.Perror(err)

	var errorCountSerious int
	var errorCountNonSerious int

	for {
		select {
		case err := <-sub.Err():
			log.Fatal(err)
		case header := <-headers:
			// New block header received. Download block with tx-receipts
			b, err := blockswithtx.GetBlockWithTxReceipts(client, header.Number.Int64())
			utils.Perror(err)

			if !silent {
				fmt.Println("Queueing new block", b.Block.Number())
			}

			// Add to backlog, because it can only be processed when the Flashbots API has caught up
			BlockBacklog[header.Number.Int64()] = b

			// Query flashbots API to get latest block it has processed
			opts := api.GetBlocksOptions{BlockNumber: header.Number.Int64()}
			flashbotsResponse, err := api.GetBlocks(&opts)
			if err != nil {
				log.Println("Flashbots API error:", err)
				continue
			}

			// Go through block-backlog, and process those within Flashbots API range
			for height, blockFromBacklog := range BlockBacklog {
				if height <= flashbotsResponse.LatestBlockNumber {
					if !silent {
						utils.PrintBlock(blockFromBacklog.Block)
					}

					check, err := blockcheck.CheckBlock(blockFromBacklog)
					if err != nil {
						log.Println("CheckBlock from backlog error:", err, "block:", blockFromBacklog.Block.Number())
						break
					}

					// no checking error, can process and remove from backlog
					delete(BlockBacklog, blockFromBacklog.Block.Number().Int64())

					// Handle errors in the bundle (print, Discord, etc.)
					if check.HasErrors() {
						if check.HasSeriousErrors() { // only serious errors are printed and sent to Discord
							errorCountSerious += 1
							msg := check.Sprint(true, false)
							print(msg)

							if sendErrorsToDiscord {
								if len(check.Errors) == 1 && check.HasBundleWith0EffectiveGasPrice {
									// Short message if only 1 error and that is a 0-effective-gas-price
									msg := check.SprintHeader(false, true)
									msg += " - Error: " + check.Errors[0]
									SendToDiscord(msg)
								} else {
									SendToDiscord(check.Sprint(false, true))
								}
							}
							fmt.Println("")
						} else if check.HasLessSeriousErrors() { // less serious errors are only counted
							errorCountNonSerious += 1
						}

						if check.HasSeriousErrors() || check.HasLessSeriousErrors() { // update and print miner error count on serious and less-serious errors
							fmt.Printf("stats - 50p_errors: %d, 25p_errors: %d\n", errorCountSerious, errorCountNonSerious)
							AddErrorCountsToMinerErrors(check)
							PrintMinerErrors()
						}
					}

					time.Sleep(1 * time.Second)
				}
			}
		}
	}
}

func AddErrorCountsToMinerErrors(check *blockcheck.BlockCheck) {
	_, found := MinerErrors[check.Miner]
	if !found {
		MinerErrors[check.Miner] = &MinerErrorCount{
			MinerHash: check.Miner,
			MinerName: check.MinerName,
			Blocks:    make(map[int64]bool),
		}
	}
	MinerErrors[check.Miner].Blocks[check.Number] = true
	MinerErrors[check.Miner].ErrorCounts.Failed0GasTx += check.ErrorCounter.Failed0GasTx
	MinerErrors[check.Miner].ErrorCounts.FailedFlashbotsTx += check.ErrorCounter.FailedFlashbotsTx
	MinerErrors[check.Miner].ErrorCounts.BundlePaysMoreThanPrevBundle += check.ErrorCounter.BundlePaysMoreThanPrevBundle
	MinerErrors[check.Miner].ErrorCounts.BundleHasLowerFeeThanLowestNonFbTx += check.ErrorCounter.BundleHasLowerFeeThanLowestNonFbTx
	MinerErrors[check.Miner].ErrorCounts.BundleHas0Fee += check.ErrorCounter.BundleHas0Fee
	MinerErrors[check.Miner].ErrorCounts.BundleHasNegativeFee += check.ErrorCounter.BundleHasNegativeFee
}

func PrintMinerErrors() {
	for k, v := range MinerErrors {
		minerInfo := k
		if v.MinerName != "" {
			minerInfo += fmt.Sprintf(" (%s)", v.MinerName)
		}
		fmt.Printf("%-66s blocks=%d \t failed0gas=%d \t failedFbTx=%d \t bundlePaysMore=%d \t bundleTooLowFee=%d \t has0fee=%d \t hasNegativeFee=%d\n", minerInfo, len(v.Blocks), v.ErrorCounts.Failed0GasTx, v.ErrorCounts.FailedFlashbotsTx, v.ErrorCounts.BundlePaysMoreThanPrevBundle, v.ErrorCounts.BundleHasLowerFeeThanLowestNonFbTx, v.ErrorCounts.BundleHas0Fee, v.ErrorCounts.BundleHasNegativeFee)
	}
}
