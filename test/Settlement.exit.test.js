const { expect } = require("chai");
const { ethers } = require("hardhat");
const { time } = require("@nomicfoundation/hardhat-network-helpers");

describe("Settlement forced exit", function () {
  let owner, operator, alice;
  let token, settlement;
  const EXIT_DELAY = 300;
  const B = ethers.parseUnits("75", 18);

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

    const leaf = await settlement.balanceLeaf(alice.address, 1, B);
    await settlement.connect(operator).commitStateRoot(leaf);
  });

  it("freezes the latest root on requestExit", async function () {
    await expect(settlement.connect(alice).requestExit(1))
      .to.emit(settlement, "ExitRequested");
    const req = await settlement.exitRequests(alice.address, 1);
    expect(req.rootIndex).to.equal(1n);
    expect(req.timestamp).to.be.greaterThan(0n);
  });

  it("finalizeExit reverts before the delay and succeeds after", async function () {
    await settlement.connect(alice).requestExit(1);
    await expect(settlement.connect(alice).finalizeExit(1, 1, B, []))
      .to.be.revertedWith("too early");

    await time.increase(EXIT_DELAY + 1);
    await expect(settlement.connect(alice).finalizeExit(1, 1, B, []))
      .to.emit(settlement, "ExitFinalized")
      .withArgs(alice.address, 1, B, 1n);

    expect(await token.balanceOf(alice.address)).to.equal(B);
    expect(await settlement.exited(alice.address, 1)).to.equal(true);
  });

  it("cannot exit twice", async function () {
    await settlement.connect(alice).requestExit(1);
    await time.increase(EXIT_DELAY + 1);
    await settlement.connect(alice).finalizeExit(1, 1, B, []);
    await expect(settlement.connect(alice).requestExit(1)).to.be.revertedWith("exited");
  });

  it("exit is allowed while paused", async function () {
    await settlement.pause();
    await settlement.connect(alice).requestExit(1);
    await time.increase(EXIT_DELAY + 1);
    await expect(settlement.connect(alice).finalizeExit(1, 1, B, []))
      .to.emit(settlement, "ExitFinalized");
  });

  it("a pending exit blocks fast withdrawal and deposit; cancel re-enables withdraw", async function () {
    await settlement.connect(alice).requestExit(1);
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 1, []))
      .to.be.revertedWith("exit pending");
    await expect(settlement.connect(alice).deposit(1, B)).to.be.revertedWith("exit pending");

    await settlement.connect(alice).cancelExit(1);
    await expect(settlement.connect(operator).withdraw(alice.address, 1, B, B, 1, []))
      .to.emit(settlement, "Withdrawal");
  });

  it("rejects finalizeExit with a wrong root or bad proof", async function () {
    await settlement.connect(alice).requestExit(1);
    await time.increase(EXIT_DELAY + 1);
    await expect(settlement.connect(alice).finalizeExit(1, 2, B, [])).to.be.revertedWith("wrong root");
    await expect(settlement.connect(alice).finalizeExit(1, 1, B + 1n, [])).to.be.revertedWith("bad proof");
  });
});
