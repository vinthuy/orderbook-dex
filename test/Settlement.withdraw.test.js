const { expect } = require("chai");
const { ethers } = require("hardhat");

describe("Settlement withdraw", function () {
  let owner, operator, alice;
  let token, settlement;
  const EXIT_DELAY = 300;
  const B = ethers.parseUnits("50", 18);

  beforeEach(async function () {
    [owner, operator, alice] = await ethers.getSigners();
    const Token = await ethers.getContractFactory("Token");
    token = await Token.deploy("Token A", "TKA", 18, 1000000);
    const Settlement = await ethers.getContractFactory("Settlement");
    settlement = await Settlement.deploy(operator.address, EXIT_DELAY);
    await settlement.registerAsset(await token.getAddress());

    await token.transfer(alice.address, B);
    await token.connect(alice).approve(await settlement.getAddress(), B);
    await settlement.connect(alice).deposit(1, B);

    // anchor a root whose only leaf is alice/asset1/B
    const leaf = await settlement.balanceLeaf(alice.address, 1, B);
    await settlement.connect(operator).commitStateRoot(leaf);
  });

  it("withdraws against the anchored root (single-leaf proof = [])", async function () {
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 1, []))
      .to.emit(settlement, "Withdrawal")
      .withArgs(alice.address, 1, B, 1n);
    expect(await token.balanceOf(alice.address)).to.equal(B);
    expect(await settlement.totalWithdrawn(1)).to.equal(B);
  });

  it("rejects a withdrawal that exceeds the proven balance", async function () {
    const tooMuch = B + 1n;
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, tooMuch, 1, []))
      .to.be.revertedWith("bad amount");
  });

  it("rejects an oversized cumulative payout from the same root", async function () {
    await settlement.connect(operator).withdraw(alice.address, 1, B, B - 1n, 1, []);
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, 2n, 1, []))
      .to.be.revertedWith("exceeds balance");
  });

  it("rejects a bad proof", async function () {
    const wrongLeaf = await settlement.balanceLeaf(alice.address, 1, B + 1n);
    // commit a root built from the wrong leaf, then try to withdraw the real balance
    await settlement.connect(operator).commitStateRoot(wrongLeaf);
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 2, []))
      .to.be.revertedWith("bad proof");
  });

  it("rejects a stale root", async function () {
    // rootIndex 0 is below the valid range [1, latestRootIndex]
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, 1n, 0, []))
      .to.be.revertedWith("bad root");

    // advance the high-water mark to root 2, then a committed older root must be rejected
    const leaf = await settlement.balanceLeaf(alice.address, 1, B);
    await settlement.connect(operator).commitStateRoot(leaf);
    await settlement.connect(operator).withdraw(alice.address, 1, B, 1n, 2, []);
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, 1n, 1, []))
      .to.be.revertedWith("stale root");
  });

  it("rejects withdraw while paused and by a non-operator", async function () {
    await expect(settlement.connect(alice).withdraw(alice.address, 1, B, B, 1, []))
      .to.be.revertedWith("not operator");
    await settlement.pause();
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 1, []))
      .to.be.revertedWith("paused");
  });
});
