const { expect } = require("chai");
const { ethers } = require("hardhat");
const path = require("path");
const fs = require("fs");

describe("Merkle cross-language parity", function () {
  let harness;
  let fixture;

  before(async function () {
    const file = path.join(__dirname, "..", "backend", "ledger", "testdata", "merkle_vectors.json");
    if (!fs.existsSync(file)) {
      throw new Error(`missing fixture ${file}; run UPDATE_GOLDEN=1 go test ./ledger/...`);
    }
    fixture = JSON.parse(fs.readFileSync(file, "utf8"));
    harness = await (await ethers.getContractFactory("MerkleLibHarness")).deploy();
  });

  it("on-chain balanceLeaf matches every Go leaf", async function () {
    for (const leaf of fixture.leaves) {
      const onChain = await harness.balanceLeaf(leaf.account, leaf.assetId, leaf.balance);
      expect(onChain).to.equal(leaf.leaf);
    }
  });

  it("on-chain verify accepts every Go proof against the Go root", async function () {
    for (let i = 0; i < fixture.proofs.length; i++) {
      const p = fixture.proofs[i];
      const leaf = fixture.leaves[p.index].leaf;
      expect(await harness.verify(p.proof, fixture.root, leaf)).to.equal(true);
    }
  });

  it("on-chain computeRoot of all Go leaves equals the Go root", async function () {
    const leaves = fixture.leaves.map((l) => l.leaf);
    expect(await harness.computeRoot(leaves)).to.equal(fixture.root);
  });
});
