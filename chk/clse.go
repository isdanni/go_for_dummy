// cleanupForceClose mines a force close commitment found in the mempool and
// the following sweep transaction from the force closing node.
func cleanupForceClose(t *harnessTest, net *lntest.NetworkHarness,
	node *lntest.HarnessNode, chanPoint *lnrpc.ChannelPoint) {
	ctxb := context.Background()

	// Wait for the channel to be marked pending force close.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err := waitForChannelPendingForceClose(ctxt, node, chanPoint)
	if err != nil {
		t.Fatalf("channel not pending force close: %v", err)
	}

	// Mine enough blocks for the node to sweep its funds from the force
	// closed channel.
	_, err = net.Miner.Node.Generate(defaultCSV)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// The node should now sweep the funds, clean up by mining the sweeping
	// tx.
	mineBlocks(t, net, 1, 1)
}
