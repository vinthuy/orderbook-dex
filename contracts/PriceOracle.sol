// SPDX-License-Identifier: MIT
pragma solidity ^0.8.19;

/// @title PriceOracle
/// @notice Publishes a signed-off index price per perpetual market, for the
///         off-chain matcher to read as its mark-price input.
/// @dev This is an OPERATOR-TRUSTED feed, not a decentralised oracle: the whole
///      point of putting it on-chain is that the price becomes a public,
///      timestamped, immutable record that anyone can audit and that the matcher
///      cannot silently rewrite — not that it is manipulation-proof.
///
///      Design decisions worth knowing:
///        * Prices are stored as `uint64` in quote minor units per whole base
///          unit, matching the engine's Price type. A zero price is rejected: the
///          matcher treats a missing price as "do not liquidate", and a zero that
///          slipped through would instead look like a valid, catastrophic price.
///        * Every write emits an event with the previous value. An auditor can
///          reconstruct the feed's full history without trusting any indexer.
///        * `setPrice` is bounded by `maxDeviationBps` against the previous price
///          so a single compromised operator transaction cannot jump the feed by
///          an unbounded amount. The bound is owner-configurable and can be set to
///          10,000 (no limit) for a cold start.
contract PriceOracle {
    struct PriceData {
        uint64 price;
        uint64 updatedAt;
    }

    address public owner;
    address public operator;
    bool public paused;

    /// @notice Maximum move from the previous price, in basis points of the
    ///         previous price. 10_000 disables the bound.
    uint16 public maxDeviationBps;

    mapping(uint32 => PriceData) private _prices;

    event PriceSet(uint32 indexed market, uint64 previous, uint64 current, uint64 timestamp);
    event OperatorSet(address indexed previous, address indexed current);
    event DeviationBoundSet(uint16 previous, uint16 current);
    event PausedSet(bool paused);

    modifier onlyOwner() {
        require(msg.sender == owner, "not owner");
        _;
    }

    modifier onlyOperator() {
        require(msg.sender == operator, "not operator");
        _;
        require(!paused, "paused");
    }

    constructor(address _operator, uint16 _maxDeviationBps) {
        require(_operator != address(0), "operator is zero");
        require(_maxDeviationBps <= 10_000, "deviation > 100%");
        owner = msg.sender;
        operator = _operator;
        maxDeviationBps = _maxDeviationBps;
    }

    /// @notice Publishes the index price for a market.
    /// @param market The perpetual market id, matching PERP_MARKETS.
    /// @param price Quote minor units per whole base unit; must be > 0.
    function setPrice(uint32 market, uint64 price) external onlyOperator {
        require(price > 0, "price is zero");

        PriceData memory prev = _prices[market];
        if (prev.price != 0 && maxDeviationBps < 10_000) {
            uint256 prevPrice = uint256(prev.price);
            uint256 diff = price > prev.price ? uint256(price) - prevPrice : prevPrice - uint256(price);
            // diff / prev <= bps / 10_000  <=>  diff * 10_000 <= prev * bps
            require(diff * 10_000 <= prevPrice * uint256(maxDeviationBps), "deviation too large");
        }

        _prices[market] = PriceData({price: price, updatedAt: uint64(block.timestamp)});
        emit PriceSet(market, prev.price, price, uint64(block.timestamp));
    }

    /// @notice Publishes several markets in one transaction.
    function setPrices(uint32[] calldata markets, uint64[] calldata prices) external onlyOperator {
        require(markets.length == prices.length, "length mismatch");
        for (uint256 i = 0; i < markets.length; i++) {
            require(prices[i] > 0, "price is zero");
            PriceData memory prev = _prices[markets[i]];
            if (prev.price != 0 && maxDeviationBps < 10_000) {
                uint256 prevPrice = uint256(prev.price);
                uint256 diff =
                    prices[i] > prev.price ? uint256(prices[i]) - prevPrice : prevPrice - uint256(prices[i]);
                require(diff * 10_000 <= prevPrice * uint256(maxDeviationBps), "deviation too large");
            }
            _prices[markets[i]] = PriceData({price: prices[i], updatedAt: uint64(block.timestamp)});
            emit PriceSet(markets[i], prev.price, prices[i], uint64(block.timestamp));
        }
    }

    /// @notice Returns the price and the timestamp of its last update. A price of
    ///         zero means the market has never been published.
    function getPrice(uint32 market) external view returns (uint64 price, uint64 updatedAt) {
        PriceData memory d = _prices[market];
        return (d.price, d.updatedAt);
    }

    /// @notice Returns the price only when it is newer than `maxAge` seconds.
    ///         Consumers must treat staleness as "no price" rather than as the
    ///         last value: a stale price is exactly what liquidates the wrong
    ///         people.
    function getFreshPrice(uint32 market, uint64 maxAge) external view returns (uint64 price, bool fresh) {
        PriceData memory d = _prices[market];
        if (d.price == 0) {
            return (0, false);
        }
        return (d.price, block.timestamp - uint256(d.updatedAt) <= maxAge);
    }

    function setOperator(address _operator) external onlyOwner {
        require(_operator != address(0), "operator is zero");
        emit OperatorSet(operator, _operator);
        operator = _operator;
    }

    function setMaxDeviationBps(uint16 _maxDeviationBps) external onlyOwner {
        require(_maxDeviationBps <= 10_000, "deviation > 100%");
        emit DeviationBoundSet(maxDeviationBps, _maxDeviationBps);
        maxDeviationBps = _maxDeviationBps;
    }

    function setPaused(bool _paused) external onlyOwner {
        paused = _paused;
        emit PausedSet(_paused);
    }
}
