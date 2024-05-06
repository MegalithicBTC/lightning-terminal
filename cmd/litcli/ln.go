package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightninglabs/taproot-assets/tapchannel"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/cmd/commands"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/urfave/cli"
)

const (
	// minAssetAmount is the minimum amount of an asset that can be put into
	// a channel. We choose an arbitrary value that allows for at least a
	// couple of HTLCs to be created without leading to fractions of assets
	// (which doesn't exist).
	minAssetAmount = 100
)

func copyCommand(command cli.Command, action interface{},
	flags ...cli.Flag) cli.Command {

	command.Flags = append(command.Flags, flags...)
	command.Action = action

	return command
}

var lnCommands = []cli.Command{
	{
		Name:     "ln",
		Usage:    "Interact with the Lightning Network.",
		Category: "Taproot Assets on LN",
		Subcommands: []cli.Command{
			fundChannelCommand,
			sendPaymentCommand,
			payInvoiceCommand,
			addInvoiceCommand,
		},
	},
}

var fundChannelCommand = cli.Command{
	Name:     "fundchannel",
	Category: "Channels",
	Usage: "Open a Taproot Asset channel with a node on the Lightning " +
		"Network.",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "node_key",
			Usage: "the identity public key of the target " +
				"node/peer serialized in compressed format, " +
				"must already be connected to",
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/vByte that should be used when crafting " +
				"the transaction",
			Value: 1,
		},
		cli.Uint64Flag{
			Name: "asset_amount",
			Usage: "The amount of the asset to commit to the " +
				"channel.",
		},
		cli.StringFlag{
			Name:  "asset_id",
			Usage: "The asset ID to commit to the channel.",
		},
	},
	Action: fundChannel,
}

func fundChannel(c *cli.Context) error {
	tapdConn, cleanup, err := connectTapdClient(c)
	if err != nil {
		return fmt.Errorf("error creating tapd connection: %w", err)
	}

	defer cleanup()

	ctxb := context.Background()
	tapdClient := taprpc.NewTaprootAssetsClient(tapdConn)
	assets, err := tapdClient.ListAssets(ctxb, &taprpc.ListAssetRequest{})
	if err != nil {
		return fmt.Errorf("error fetching assets: %w", err)
	}

	assetIDBytes, err := hex.DecodeString(c.String("asset_id"))
	if err != nil {
		return fmt.Errorf("error hex decoding asset ID: %w", err)
	}

	requestedAmount := c.Uint64("asset_amount")
	if requestedAmount < minAssetAmount {
		return fmt.Errorf("requested amount must be at least %d",
			minAssetAmount)
	}

	nodePubBytes, err := hex.DecodeString(c.String("node_key"))
	if err != nil {
		return fmt.Errorf("unable to decode node public key: %w", err)
	}

	assetFound := false
	for _, rpcAsset := range assets.Assets {
		if !bytes.Equal(rpcAsset.AssetGenesis.AssetId, assetIDBytes) {
			continue
		}

		if rpcAsset.Amount < requestedAmount {
			continue
		}

		assetFound = true
	}

	if !assetFound {
		return fmt.Errorf("asset with ID %x not found or no UTXO with "+
			"at least amount %d is available", assetIDBytes,
			requestedAmount)
	}

	resp, err := tapdClient.FundChannel(
		ctxb, &taprpc.FundChannelRequest{
			Amount:             requestedAmount,
			AssetId:            assetIDBytes,
			PeerPubkey:         nodePubBytes,
			FeeRateSatPerVbyte: uint32(c.Uint64("sat_per_vbyte")),
		},
	)
	if err != nil {
		return fmt.Errorf("error funding channel: %w", err)
	}

	printJSON(resp)

	return nil
}

type assetBalance struct {
	AssetID       string
	Name          string
	LocalBalance  uint64
	RemoteBalance uint64
	ChannelID     uint64
	PeerPubKey    string
}

type channelBalResp struct {
	Assets map[string]*assetBalance `json:"assets"`
}

func computeAssetBalances(lnd lnrpc.LightningClient) (*channelBalResp, error) {
	ctxb := context.Background()
	openChans, err := lnd.ListChannels(
		ctxb, &lnrpc.ListChannelsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channels: %w", err)
	}

	balanceResp := &channelBalResp{
		Assets: make(map[string]*assetBalance),
	}
	for _, openChan := range openChans.Channels {
		if len(openChan.CustomChannelData) == 0 {
			continue
		}

		var assetData tapchannel.JsonAssetChannel
		err = json.Unmarshal(openChan.CustomChannelData, &assetData)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal asset "+
				"data: %w", err)
		}

		for _, assetOutput := range assetData.Assets {
			assetID := assetOutput.AssetInfo.AssetGenesis.AssetID
			assetName := assetOutput.AssetInfo.AssetGenesis.Name

			balance, ok := balanceResp.Assets[assetID]
			if !ok {
				balance = &assetBalance{
					AssetID:    assetID,
					Name:       assetName,
					ChannelID:  openChan.ChanId,
					PeerPubKey: openChan.RemotePubkey,
				}
				balanceResp.Assets[assetID] = balance
			}

			balance.LocalBalance += assetOutput.LocalBalance
			balance.RemoteBalance += assetOutput.RemoteBalance
		}
	}

	return balanceResp, nil
}

var (
	assetIDFlag = cli.StringFlag{
		Name: "asset_id",
		Usage: "the asset ID of the asset to use when sending " +
			"payments with assets",
	}
)

var sendPaymentCommand = cli.Command{
	Name:     "sendpayment",
	Category: commands.SendPaymentCommand.Category,
	Usage: "Send a payment over Lightning, potentially using a " +
		"mulit-asset channel as the first hop",
	Description: commands.SendPaymentCommand.Description + `
	To send an multi-asset LN payment to a single hop, the --asset_id=X
	argument should be used.

	Note that this will only work in concert with the --keysend argument.
	`,
	ArgsUsage: commands.SendPaymentCommand.ArgsUsage + " --asset_id=X",
	Flags:     append(commands.SendPaymentCommand.Flags, assetIDFlag),
	Action:    sendPayment,
}

func sendPayment(ctx *cli.Context) error {
	// Show command help if no arguments provided
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "sendpayment")
		return nil
	}

	lndConn, cleanup, err := connectClient(ctx, false)
	if err != nil {
		return fmt.Errorf("unable to make rpc con: %w", err)
	}

	defer cleanup()

	lndClient := lnrpc.NewLightningClient(lndConn)

	switch {
	case !ctx.IsSet(assetIDFlag.Name):
		return fmt.Errorf("the --asset_id flag must be set")
	case !ctx.IsSet("keysend"):
		return fmt.Errorf("the --keysend flag must be set")
	case !ctx.IsSet("amt"):
		return fmt.Errorf("--amt must be set")
	}

	assetIDStr := ctx.String(assetIDFlag.Name)

	assetIDBytes, err := hex.DecodeString(assetIDStr)
	if err != nil {
		return fmt.Errorf("unable to decode assetID: %v", err)
	}

	// First, based on the asset ID and amount, we'll make sure that this
	// channel even has enough funds to send.
	assetBalances, err := computeAssetBalances(lndClient)
	if err != nil {
		return fmt.Errorf("unable to compute asset balances: %w", err)
	}

	balance, ok := assetBalances.Assets[assetIDStr]
	if !ok {
		return fmt.Errorf("unable to send asset_id=%v, not in "+
			"channel", assetIDStr)
	}

	amtToSend := ctx.Uint64("amt")
	if amtToSend > balance.LocalBalance {
		return fmt.Errorf("insufficient balance, want to send %v, "+
			"only have %v", amtToSend, balance.LocalBalance)
	}

	var assetID asset.ID
	copy(assetID[:], assetIDBytes)

	// Now that we know the amount we need to send, we'll convert that into
	// an HTLC tlv, which'll be used as the first hop TLV value.
	assetAmts := []*tapchannel.AssetBalance{
		tapchannel.NewAssetBalance(assetID, amtToSend),
	}

	htlc := tapchannel.NewHtlc(assetAmts, tapchannel.NoneRfqID())

	// We'll now map the HTLC struct into a set of TLV records, which we
	// can then encode into the map format expected.
	htlcMapRecords, err := tlv.RecordsToMap(htlc.Records())
	if err != nil {
		return fmt.Errorf("unable to encode records as map: %w", err)
	}

	// With the asset specific work out of the way, we'll parse the rest of
	// the command as normal.
	var (
		destNode []byte
		rHash    []byte
	)

	switch {
	case ctx.IsSet("dest"):
		destNode, err = hex.DecodeString(ctx.String("dest"))
	default:
		return fmt.Errorf("destination txid argument missing")
	}
	if err != nil {
		return err
	}

	if len(destNode) != 33 {
		return fmt.Errorf("dest node pubkey must be exactly 33 bytes, is "+
			"instead: %v", len(destNode))
	}

	// We use a constant amount of 500 to carry the asset HTLCs. In the
	// future, we can use the double HTLC trick here, though it consumes
	// more commitment space.
	const htlcCarrierAmt = 500
	req := &routerrpc.SendPaymentRequest{
		Dest:                  destNode,
		Amt:                   htlcCarrierAmt,
		DestCustomRecords:     make(map[uint64][]byte),
		FirstHopCustomRecords: htlcMapRecords,
	}

	if ctx.IsSet("payment_hash") {
		return errors.New("cannot set payment hash when using " +
			"keysend")
	}

	// Read out the custom preimage for the keysend payment.
	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		return err
	}

	// Set the preimage. If the user supplied a preimage with the data
	// flag, the preimage that is set here will be overwritten later.
	req.DestCustomRecords[record.KeySendType] = preimage[:]

	hash := preimage.Hash()
	rHash = hash[:]

	req.PaymentHash = rHash

	return commands.SendPaymentRequest(ctx, req)
}

var payInvoiceCommand = cli.Command{
	Name:     "payinvoice",
	Category: "Payments",
	Usage:    "Pay an invoice over lightning using an asset.",
	Description: `
	This command attempts to pay an invoice using an asset channel as the
	source of the payment. The asset ID of the channel must be specified
	using the --asset_id flag.
	`,
	ArgsUsage: "pay_req --asset_id=X",
	Flags: append(commands.PaymentFlags(),
		cli.Int64Flag{
			Name: "amt",
			Usage: "(optional) number of satoshis to fulfill the " +
				"invoice",
		},
		assetIDFlag,
	),
	Action: payInvoice,
}

func payInvoice(ctx *cli.Context) error {
	args := ctx.Args()
	ctxb := context.Background()

	var payReq string
	switch {
	case ctx.IsSet("pay_req"):
		payReq = ctx.String("pay_req")
	case args.Present():
		payReq = args.First()
	default:
		return fmt.Errorf("pay_req argument missing")
	}

	lndConn, cleanup, err := connectClient(ctx, false)
	if err != nil {
		return fmt.Errorf("unable to make rpc con: %w", err)
	}

	defer cleanup()

	lndClient := lnrpc.NewLightningClient(lndConn)

	decodeReq := &lnrpc.PayReqString{PayReq: payReq}
	decodeResp, err := lndClient.DecodePayReq(ctxb, decodeReq)
	if err != nil {
		return err
	}

	if !ctx.IsSet(assetIDFlag.Name) {
		return fmt.Errorf("the --asset_id flag must be set")
	}

	assetIDStr := ctx.String(assetIDFlag.Name)

	assetIDBytes, err := hex.DecodeString(assetIDStr)
	if err != nil {
		return fmt.Errorf("unable to decode assetID: %v", err)
	}

	// First, based on the asset ID and amount, we'll make sure that this
	// channel even has enough funds to send.
	assetBalances, err := computeAssetBalances(lndClient)
	if err != nil {
		return fmt.Errorf("unable to compute asset balances: %w", err)
	}

	balance, ok := assetBalances.Assets[assetIDStr]
	if !ok {
		return fmt.Errorf("unable to send asset_id=%v, not in "+
			"channel", assetIDStr)
	}

	if balance.LocalBalance == 0 {
		return fmt.Errorf("no asset balance available for asset_id=%v",
			assetIDStr)
	}

	var assetID asset.ID
	copy(assetID[:], assetIDBytes)

	tapdConn, cleanup, err := connectTapdClient(ctx)
	if err != nil {
		return fmt.Errorf("error creating tapd connection: %w", err)
	}

	defer cleanup()

	peerPubKey, err := hex.DecodeString(balance.PeerPubKey)
	if err != nil {
		return fmt.Errorf("unable to decode peer pubkey: %w", err)
	}

	rfqClient := rfqrpc.NewRfqClient(tapdConn)

	timeoutSeconds := uint32(60)
	fmt.Printf("Asking peer %x for quote to sell assets to pay for "+
		"invoice over %d msats; waiting up to %ds\n", peerPubKey,
		decodeResp.NumMsat, timeoutSeconds)

	resp, err := rfqClient.AddAssetSellOrder(
		ctxb, &rfqrpc.AddAssetSellOrderRequest{
			AssetSpecifier: &rfqrpc.AssetSpecifier{
				Id: &rfqrpc.AssetSpecifier_AssetIdStr{
					AssetIdStr: assetIDStr,
				},
			},
			MaxAssetAmount: balance.LocalBalance,
			MinAsk:         uint64(decodeResp.NumMsat),
			Expiry:         uint64(decodeResp.Expiry),
			PeerPubKey:     peerPubKey,
			TimeoutSeconds: timeoutSeconds,
		},
	)
	if err != nil {
		return fmt.Errorf("error adding sell order: %w", err)
	}

	msatPerUnit := resp.AcceptedQuote.BidPrice
	numUnits := uint64(decodeResp.NumMsat) / msatPerUnit

	fmt.Printf("Got quote for %v asset units at %v msat/unit from peer "+
		"%x with SCID %d\n", numUnits, msatPerUnit, peerPubKey,
		resp.AcceptedQuote.Scid)

	var rfqID rfqmsg.ID
	copy(rfqID[:], resp.AcceptedQuote.Id)
	htlc := tapchannel.NewHtlc(nil, tapchannel.SomeRfqID(rfqID))

	// We'll now map the HTLC struct into a set of TLV records, which we
	// can then encode into the map format expected.
	htlcMapRecords, err := tlv.RecordsToMap(htlc.Records())
	if err != nil {
		return fmt.Errorf("unable to encode records as map: %w", err)
	}

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest:        commands.StripPrefix(payReq),
		FirstHopCustomRecords: htlcMapRecords,
	}

	return commands.SendPaymentRequest(ctx, req)
}

var addInvoiceCommand = cli.Command{
	Name:     "addinvoice",
	Category: commands.AddInvoiceCommand.Category,
	Usage:    "Add a new invoice to receive Taproot Assets.",
	Description: `
	Add a new invoice, expressing intent for a future payment, received in
	Taproot Assets.
	`,
	ArgsUsage: "asset_id asset_amount",
	Flags: append(
		commands.AddInvoiceCommand.Flags,
		cli.StringFlag{
			Name:  "asset_id",
			Usage: "the asset ID of the asset to receive",
		},
		cli.Uint64Flag{
			Name:  "asset_amount",
			Usage: "the amount of assets to receive",
		},
	),
	Action: addInvoice,
}

func addInvoice(ctx *cli.Context) error {
	args := ctx.Args()
	ctxb := context.Background()

	var assetIDStr string
	switch {
	case ctx.IsSet("asset_id"):
		assetIDStr = ctx.String("asset_id")
	case args.Present():
		assetIDStr = args.First()
		args = args.Tail()
	default:
		return fmt.Errorf("asset_id argument missing")
	}

	var (
		assetAmount uint64
		err         error
	)
	switch {
	case ctx.IsSet("asset_amount"):
		assetAmount = ctx.Uint64("asset_amount")
	case args.Present():
		assetAmount, err = strconv.ParseUint(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to parse asset amount %w",
				err)
		}
	default:
		return fmt.Errorf("asset_amount argument missing")
	}

	expiry := time.Now().Add(300 * time.Second)
	if ctx.IsSet("expiry") {
		expirySeconds := ctx.Uint64("expiry")
		expiry = time.Now().Add(
			time.Duration(expirySeconds) * time.Second,
		)
	}

	lndConn, cleanup, err := connectClient(ctx, false)
	if err != nil {
		return fmt.Errorf("unable to make rpc con: %w", err)
	}

	defer cleanup()

	lndClient := lnrpc.NewLightningClient(lndConn)

	assetIDBytes, err := hex.DecodeString(assetIDStr)
	if err != nil {
		return fmt.Errorf("unable to decode assetID: %v", err)
	}

	// First, based on the asset ID and amount, we'll make sure that this
	// channel even has enough funds to send.
	assetBalances, err := computeAssetBalances(lndClient)
	if err != nil {
		return fmt.Errorf("unable to compute asset balances: %w", err)
	}

	balance, ok := assetBalances.Assets[assetIDStr]
	if !ok {
		return fmt.Errorf("unable to send asset_id=%v, not in "+
			"channel", assetIDStr)
	}

	if balance.RemoteBalance == 0 {
		return fmt.Errorf("no remote asset balance available for "+
			"receiving asset_id=%v", assetIDStr)
	}

	var assetID asset.ID
	copy(assetID[:], assetIDBytes)

	tapdConn, cleanup, err := connectTapdClient(ctx)
	if err != nil {
		return fmt.Errorf("error creating tapd connection: %w", err)
	}

	defer cleanup()

	peerPubKey, err := hex.DecodeString(balance.PeerPubKey)
	if err != nil {
		return fmt.Errorf("unable to decode peer pubkey: %w", err)
	}

	rfqClient := rfqrpc.NewRfqClient(tapdConn)

	timeoutSeconds := uint32(60)
	fmt.Printf("Asking peer %x for quote to buy assets to receive for "+
		"invoice over %d units; waiting up to %ds\n", peerPubKey,
		assetAmount, timeoutSeconds)

	resp, err := rfqClient.AddAssetBuyOrder(
		ctxb, &rfqrpc.AddAssetBuyOrderRequest{
			AssetSpecifier: &rfqrpc.AssetSpecifier{
				Id: &rfqrpc.AssetSpecifier_AssetIdStr{
					AssetIdStr: assetIDStr,
				},
			},
			MinAssetAmount: assetAmount,
			Expiry:         uint64(expiry.Unix()),
			PeerPubKey:     peerPubKey,
			TimeoutSeconds: timeoutSeconds,
		},
	)
	if err != nil {
		return fmt.Errorf("error adding sell order: %w", err)
	}

	msatPerUnit := resp.AcceptedQuote.AskPrice
	numMSats := lnwire.MilliSatoshi(assetAmount * msatPerUnit)

	descHash, err := hex.DecodeString(ctx.String("description_hash"))
	if err != nil {
		return fmt.Errorf("unable to parse description_hash: %w", err)
	}

	invoice := &lnrpc.Invoice{
		Memo:            ctx.String("memo"),
		ValueMsat:       int64(numMSats),
		DescriptionHash: descHash,
		FallbackAddr:    ctx.String("fallback_addr"),
		Expiry:          ctx.Int64("expiry"),
		Private:         ctx.Bool("private"),
		IsAmp:           ctx.Bool("amp"),
		RouteHints: []*lnrpc.RouteHint{
			{
				HopHints: []*lnrpc.HopHint{
					{
						ChanId: resp.AcceptedQuote.Scid,
						NodeId: balance.PeerPubKey,
					},
				},
			},
		},
	}

	invoiceResp, err := lndClient.AddInvoice(ctxb, invoice)
	if err != nil {
		return err
	}

	printRespJSON(invoiceResp)

	return nil
}
