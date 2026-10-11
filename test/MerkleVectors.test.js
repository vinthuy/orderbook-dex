const fs = require("fs");
const path = require("path");
const { expect } = require("chai");
const { ethers } = require("hardhat");

const fixturePath = path.join(
  __dirname,
  "..",
  "backend",
  "ledger",
  "testdata",
  "merkle_vectors.json"
);

describe("MerkleVectors (cross-language golden)", function () {
  let harness;
  const fixture = JSON.parse(fs.readFileSync(fixturePath, "utf8"));

  before(async function () {
    harness = await (await ethers.getContractFactory("MerkleLibHarness")).deploy();
    await harness.waitForDeployment();
  });

  it("每个 Go 叶子匹配链上 balanceLeaf", async function () {
    for (const leaf of fixture.leaves) {
      const got = await harness.balanceLeaf(leaf.account, leaf.assetId, leaf.balance);
      expect(got).to.equal(leaf.leaf, `leaf mismatch for ${leaf.account}/${leaf.assetId}`);
    }
  });

  it("每个 Go 证明在链上根上验证通过", async function () {
    for (const p of fixture.proofs) {
      const leaf = fixture.leaves[p.index].leaf;
      expect(await harness.verify(p.proof, fixture.root, leaf)).to.equal(true);
    }
  });
});
