const { expect } = require("chai");
const { ethers } = require("hardhat");

describe("Settlement core", function () {
  let owner, operator, alice;
  let token, settlement;
  const EXIT_DELAY = 300;

  beforeEach(async function () {
    [owner, operator, alice] = await ethers.getSigners();
    const Token = await ethers.getContractFactory("Token");
    token = await Token.deploy("Token A", "TKA", 18, 1000000);
    const Settlement = await ethers.getContractFactory("Settlement");
    settlement = await Settlement.deploy(operator.address, EXIT_DELAY);
    await settlement.registerAsset(await token.getAddress());
  });

  it("registers an asset with id 1", async function () {
    expect(await settlement.assetCount()).to.equal(1n);
    expect(await settlement.assets(1)).to.equal(await token.getAddress());
    expect(await settlement.assetIds(await token.getAddress())).to.equal(1n);
  });

  it("rejects registering the same asset twice", async function () {
    await expect(settlement.registerAsset(await token.getAddress()))
      .to.be.revertedWith("already registered");
  });

  it("only owner can register assets", async function () {
    await expect(settlement.connect(alice).registerAsset(await token.getAddress()))
      .to.be.revertedWith("not owner");
  });

  it("deposits and emits Deposit with an incrementing seq", async function () {
    const amt = ethers.parseUnits("100", 18);
    await token.transfer(alice.address, amt * 2n);
    await token.connect(alice).approve(await settlement.getAddress(), amt * 2n);

    await expect(settlement.connect(alice).deposit(1, amt))
      .to.emit(settlement, "Deposit")
      .withArgs(alice.address, 1, amt, 1n);

    await expect(settlement.connect(alice).deposit(1, amt))
      .to.emit(settlement, "Deposit")
      .withArgs(alice.address, 1, amt, 2n);

    expect(await token.balanceOf(await settlement.getAddress())).to.equal(amt * 2n);
    expect(await settlement.totalDeposited(1)).to.equal(amt * 2n);
  });

  it("rejects deposit when paused or for an unknown asset", async function () {
    await settlement.pause();
    await expect(settlement.connect(alice).deposit(1, 1n)).to.be.revertedWith("paused");
    await settlement.unpause();
    await expect(settlement.connect(alice).deposit(2, 1n)).to.be.revertedWith("unknown asset");
  });

  it("operator commits a state root and rootIndex increments", async function () {
    const root = ethers.keccak256("0x1234");
    await expect(settlement.connect(operator).commitStateRoot(root))
      .to.emit(settlement, "StateRootCommitted");
    const [stored, ts] = await settlement.rootAt(1);
    expect(stored).to.equal(root);
    expect(ts).to.be.greaterThan(0n);
    expect(await settlement.latestRootIndex()).to.equal(1n);
  });

  it("rejects commit from a non-operator", async function () {
    await expect(settlement.connect(alice).commitStateRoot(ethers.ZeroHash))
      .to.be.revertedWith("not operator");
  });

  it("rejects commit when the vault is insolvent", async function () {
    const Mock = await ethers.getContractFactory("MockToken");
    const mock = await Mock.deploy(ethers.parseUnits("1000", 18));
    await settlement.registerAsset(await mock.getAddress()); // assetId 2

    await mock.setNoOp(true); // transferFrom succeeds but moves nothing
    await mock.approve(await settlement.getAddress(), ethers.parseUnits("100", 18));
    await settlement.deposit(2, ethers.parseUnits("100", 18));

    expect(await settlement.totalDeposited(2)).to.equal(ethers.parseUnits("100", 18));
    await expect(settlement.connect(operator).commitStateRoot(ethers.ZeroHash))
      .to.be.revertedWith("insolvent");
  });

  it("owner can change the operator", async function () {
    await expect(settlement.setOperator(alice.address))
      .to.emit(settlement, "OperatorChanged")
      .withArgs(operator.address, alice.address);
    expect(await settlement.operator()).to.equal(alice.address);
  });
});
