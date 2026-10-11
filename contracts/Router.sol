// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

import "./Token.sol";
import "./AMMPool.sol";
import "./Factory.sol";

contract Router {
    Factory public factory;

    constructor(address _factory) {
        factory = Factory(_factory);
    }

    function addLiquidity(
        address token0Addr,
        address token1Addr,
        uint256 amount0,
        uint256 amount1
    ) external returns (uint256 liquidity) {
        address pool = factory.getPool(token0Addr, token1Addr);
        require(pool != address(0), "Pool not found");

        Token(token0Addr).transfer(pool, amount0);
        Token(token1Addr).transfer(pool, amount1);

        AMMPool(pool).mint(msg.sender);
    }

    function removeLiquidity(
        address token0Addr,
        address token1Addr,
        uint256 liquidity
    ) external returns (uint256 amount0, uint256 amount1) {
        address pool = factory.getPool(token0Addr, token1Addr);
        require(pool != address(0), "Pool not found");

        (amount0, amount1) = AMMPool(pool).removeLiquidity(liquidity, msg.sender);
    }

    function swap(
        address tokenIn,
        address tokenOut,
        uint256 amountIn,
        uint256 amountOutMin
    ) external {
        address pool = factory.getPool(tokenIn, tokenOut);
        require(pool != address(0), "Pool not found");

        AMMPool(pool).swap(tokenIn, amountIn, amountOutMin, msg.sender);
    }

    function getPoolInfo(address poolAddr) external view returns (
        address token0,
        address token1,
        uint256 reserve0,
        uint256 reserve1,
        uint256 totalSupply
    ) {
        AMMPool p = AMMPool(poolAddr);
        token0 = address(p.token0());
        token1 = address(p.token1());
        (reserve0, reserve1) = p.getReserves();
        totalSupply = p.totalSupply();
    }
}
