// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

import "./interfaces/IFactory.sol";
import "./Token.sol";
import "./AMMPool.sol";

contract Factory is IFactory {
    mapping(address => mapping(address => address)) public override getPool;
    address[] public allPools;

    function createPool(address token0, address token1) external override returns (address pool) {
        require(token0 != token1, "Same token");
        require(getPool[token0][token1] == address(0), "Pool exists");

        AMMPool ammPool = new AMMPool(Token(token0), Token(token1));
        pool = address(ammPool);
        getPool[token0][token1] = pool;
        getPool[token1][token0] = pool;
        allPools.push(pool);
        
        uint256 poolId = allPools.length;
        emit PoolCreated(token0, token1, pool, poolId);
        
        return pool;
    }

    function getAllPools() external view override returns (address[] memory) {
        return allPools;
    }
}
