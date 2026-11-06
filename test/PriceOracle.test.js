const { expect } = require("chai");
const { ethers } = require("hardhat");
const { time } = require("@nomicfoundation/hardhat-network-helpers");

describe("PriceOracle", function () {
  let owner, operator, alice, oracle;
  const BPS = 500n; // 5% max deviation

  beforeEach(async function () {
    [owner, operator, alice] = await ethers.getSigners();
    const Oracle = await ethers.getContractFactory("PriceOracle");
    oracle = await Oracle.deploy(operator.address, BPS);
    await oracle.waitForDeployment();
  });

  it("rejects a zero operator", async function () {
    const Oracle = await ethers.getContractFactory("PriceOracle");
    await expect(Oracle.deploy(ethers.ZeroAddress, BPS)).to.be.revertedWith("operator is zero");
  });

  it("rejects a deviation bound above 100%", async function () {
    const Oracle = await ethers.getContractFactory("PriceOracle");
    await expect(Oracle.deploy(operator.address, 10_001)).to.be.revertedWith("deviation > 100%");
  });

  it("stores a price and emits the previous value", async function () {
    await expect(oracle.connect(operator).setPrice(1, 2000))
      .to.emit(oracle, "PriceSet")
      .withArgs(1, 0, 2000, (v) => v > 0);

    const [price, updatedAt] = await oracle.getPrice(1);
    expect(price).to.equal(2000n);
    expect(updatedAt).to.be.greaterThan(0n);
  });

  it("rejects a zero price", async function () {
    await expect(oracle.connect(operator).setPrice(1, 0)).to.be.revertedWith("price is zero");
  });

  it("only lets the operator publish", async function () {
    await expect(oracle.connect(alice).setPrice(1, 2000)).to.be.revertedWith("not operator");
  });

  it("refuses a move beyond the deviation bound", async function () {
    await oracle.connect(operator).setPrice(1, 2000);
    // 5% of 2000 is 100, so 2101 is out of bounds and 2100 is exactly at it.
    await expect(oracle.connect(operator).setPrice(1, 2101)).to.be.revertedWith("deviation too large");
    await expect(oracle.connect(operator).setPrice(1, 2100)).to.not.be.reverted;
  });

  it("allows an unbounded move when the bound is 10_000", async function () {
    await oracle.connect(owner).setMaxDeviationBps(10_000);
    await oracle.connect(operator).setPrice(1, 2000);
    await expect(oracle.connect(operator).setPrice(1, 20_000)).to.not.be.reverted;
  });

  it("publishes several markets atomically", async function () {
    await oracle.connect(operator).setPrices([1, 2], [2000, 3000]);
    expect((await oracle.getPrice(1))[0]).to.equal(2000n);
    expect((await oracle.getPrice(2))[0]).to.equal(3000n);
    await expect(oracle.connect(operator).setPrices([1, 2], [2000])).to.be.revertedWith("length mismatch");
  });

  it("reports freshness rather than silently returning a stale price", async function () {
    await oracle.connect(operator).setPrice(1, 2000);
    let [price, fresh] = await oracle.getFreshPrice(1, 60);
    expect(price).to.equal(2000n);
    expect(fresh).to.equal(true);

    await time.increase(61);
    [price, fresh] = await oracle.getFreshPrice(1, 60);
    expect(price).to.equal(2000n);
    expect(fresh).to.equal(false);
  });

  it("reports an unpublished market as not fresh", async function () {
    const [price, fresh] = await oracle.getFreshPrice(99, 60);
    expect(price).to.equal(0n);
    expect(fresh).to.equal(false);
  });

  it("stops publishing while paused and resumes after", async function () {
    await oracle.connect(owner).setPaused(true);
    await expect(oracle.connect(operator).setPrice(1, 2000)).to.be.revertedWith("paused");
    await oracle.connect(owner).setPaused(false);
    await expect(oracle.connect(operator).setPrice(1, 2000)).to.not.be.reverted;
  });

  it("only lets the owner rotate the operator", async function () {
    await expect(oracle.connect(alice).setOperator(alice.address)).to.be.revertedWith("not owner");
    await oracle.connect(owner).setOperator(alice.address);
    await expect(oracle.connect(operator).setPrice(1, 2000)).to.be.revertedWith("not operator");
    await expect(oracle.connect(alice).setPrice(1, 2000)).to.not.be.reverted;
  });
});
