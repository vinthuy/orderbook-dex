const { expect } = require("chai");
const { ethers } = require("hardhat");

describe("Enhanced Factory", function () {
    let factory, tokenA, tokenB;

    beforeEach(async function () {
        const Token = await ethers.getContractFactory("Token");
        tokenA = await Token.deploy("Token A", "TKA", 18, 1000000);
        await tokenA.waitForDeployment();
        tokenB = await Token.deploy("Token B", "TKB", 18, 1000000);
        await tokenB.waitForDeployment();

        const Factory = await ethers.getContractFactory("Factory");
        factory = await Factory.deploy();
        await factory.waitForDeployment();
    });

    it("Should create pool and emit event", async function () {
        const tx = await factory.createPool(tokenA.target, tokenB.target);
        const receipt = await tx.wait();

        const events = receipt.logs
            .map((log) => {
                try { return factory.interface.parseLog(log); } catch { return null; }
            })
            .filter(Boolean);

        const event = events.find((e) => e.name === "PoolCreated");
        expect(event).to.not.be.undefined;
        expect(event.args.token0).to.equal(tokenA.target);
        expect(event.args.token1).to.equal(tokenB.target);
        expect(event.args.pool).to.be.properAddress;
        expect(event.args.poolId).to.equal(1n);
    });

    it("Should list all pools", async function () {
        await factory.createPool(tokenA.target, tokenB.target);
        const pools = await factory.getAllPools();
        expect(pools.length).to.equal(1);
    });
});
