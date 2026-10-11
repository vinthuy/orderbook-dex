const { expect } = require("chai");
const { ethers } = require("hardhat");

const hashPair = (a, b) => {
  const [x, y] = BigInt(a) <= BigInt(b) ? [a, b] : [b, a];
  return ethers.solidityPackedKeccak256(["bytes32", "bytes32"], [x, y]);
};

const buildRoot = (leaves) => {
  if (leaves.length === 0) return ethers.ZeroHash;
  let level = leaves.slice();
  while (level.length > 1) {
    const next = [];
    for (let i = 0; i < level.length; i += 2) {
      if (i + 1 === level.length) next.push(level[i]);
      else next.push(hashPair(level[i], level[i + 1]));
    }
    level = next;
  }
  return level[0];
};

const buildProof = (leaves, index) => {
  const proof = [];
  let level = leaves.slice();
  let idx = index;
  while (level.length > 1) {
    if (idx % 2 === 0) {
      if (idx + 1 < level.length) proof.push(level[idx + 1]);
    } else {
      proof.push(level[idx - 1]);
    }
    const next = [];
    for (let i = 0; i < level.length; i += 2) {
      if (i + 1 === level.length) next.push(level[i]);
      else next.push(hashPair(level[i], level[i + 1]));
    }
    level = next;
    idx = Math.floor(idx / 2);
  }
  return proof;
};

describe("MerkleLib", function () {
  let harness;
  const account = "0x1111111111111111111111111111111111111111";

  before(async function () {
    harness = await (await ethers.getContractFactory("MerkleLibHarness")).deploy();
  });

  it("balanceLeaf matches packed encoding", async function () {
    const expected = ethers.solidityPackedKeccak256(
      ["address", "uint256", "uint256"],
      [account, 1, 1000n]
    );
    expect(await harness.balanceLeaf(account, 1, 1000n)).to.equal(expected);
  });

  it("single-leaf root equals the leaf", async function () {
    const leaf = await harness.balanceLeaf(account, 1, 100);
    expect(await harness.computeRoot([leaf])).to.equal(leaf);
  });

  it("empty set root is zero", async function () {
    expect(await harness.computeRoot([])).to.equal(ethers.ZeroHash);
  });

  it("3-leaf root matches the JS reference (odd node promoted)", async function () {
    const leaves = [
      await harness.balanceLeaf(account, 1, 100),
      await harness.balanceLeaf(account, 2, 200),
      await harness.balanceLeaf(account, 3, 300),
    ];
    expect(await harness.computeRoot(leaves)).to.equal(buildRoot(leaves));
  });

  it("verifies each valid proof and rejects a wrong leaf", async function () {
    const leaves = [
      await harness.balanceLeaf(account, 1, 100),
      await harness.balanceLeaf(account, 2, 200),
      await harness.balanceLeaf(account, 3, 300),
    ];
    const root = buildRoot(leaves);
    for (let i = 0; i < leaves.length; i++) {
      expect(await harness.verify(buildProof(leaves, i), root, leaves[i])).to.equal(true);
    }
    const wrong = await harness.balanceLeaf(account, 9, 999);
    expect(await harness.verify(buildProof(leaves, 0), root, wrong)).to.equal(false);
  });
});
