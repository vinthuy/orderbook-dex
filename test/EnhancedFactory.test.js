const { expect } = require("chai");
const { ethers } = require("hardhat");

describe("Enhanced Factory", function () {
    let factory, tokenA, tokenB;
    
    beforeEach(async function () {
        const Token = await ethers.getContractFactory("Token");
        tokenA = await Token.deploy("Token A", "TKA", 18, 1000000);
        tokenB = await Token.deploy("Token B", "TKB", 18, 1000000);
        
        const Factory = await ethers.getContractFactory("Factory");
        factory = await Factory.deploy();
    });
    
    it("Should create pool and emit event", async function () {
        const tx = await factory.createPool(tokenA.address, tokenB.address);
        const receipt = await tx.wait();
        const event = receipt.events.find(e => e.event === "PoolCreated");
        expect(event).to.not.be.undefined;
        expect(event.args.token0).to.equal(tokenA.address);
        expect(event.args.token1).to.equal(tokenB.address);
        expect(event.args.pool).to.be.properAddress;
        expect(event.args.poolId).to.equal(1);
    });
    
    it("Should list all pools", async function () {
        await factory.createPool(tokenA.address, tokenB.address);
        const pools = await factory.getAllPools();
        expect(pools.length).to.equal(1);
    });
});
