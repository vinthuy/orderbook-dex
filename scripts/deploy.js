const { ethers } = require("hardhat");

async function main() {
  console.log("Deploying AMM DEX contracts...");

  const [deployer] = await ethers.getSigners();
  console.log("Deploying with account:", deployer.address);
  console.log("Account balance:", (await deployer.getBalance()).toString());

  const Token = await ethers.getContractFactory("Token");
  const Factory = await ethers.getContractFactory("Factory");
  const Router = await ethers.getContractFactory("Router");

  const USDT = await Token.deploy("Tether USD", "USDT", 6, 1000000);
  await USDT.waitForDeployment();
  console.log("USDT deployed to:", await USDT.getAddress());

  const DAI = await Token.deploy("Dai Stablecoin", "DAI", 18, 1000000);
  await DAI.waitForDeployment();
  console.log("DAI deployed to:", await DAI.getAddress());

  const FactoryContract = await Factory.deploy();
  await FactoryContract.waitForDeployment();
  console.log("Factory deployed to:", await FactoryContract.getAddress());

  const RouterContract = await Router.deploy(await FactoryContract.getAddress());
  await RouterContract.waitForDeployment();
  console.log("Router deployed to:", await RouterContract.getAddress());

  console.log("\nDeployment complete!");
  console.log(JSON.stringify({
    tokens: { USDT: await USDT.getAddress(), DAI: await DAI.getAddress() },
    factory: await FactoryContract.getAddress(),
    router: await RouterContract.getAddress()
  }, null, 2));
}

main()
  .then(() => process.exit(0))
  .catch((error) => {
    console.error(error);
    process.exit(1);
  });
