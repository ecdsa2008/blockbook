package common

import (
	"encoding/json"
	"os"

	"github.com/juju/errors"
)

// Config struct
// CoinName: 加密货币的全名，如"Bitcoin"、"Ethereum"等。这个名称用于标识正在操作的具体货币。
// CoinShortcut: 加密货币的缩写，例如BTC表示比特币，ETH表示以太坊。这是加密货币更常用的简短标识。
// CoinLabel: 加密货币的标签或别名，可能用于用户界面显示，以提供更友好或更具描述性的货币名称。
// FourByteSignatures: 可能指的是以太坊交易中的四字节签名，这些签名用于标识智能合约的函数调用。这个字段可能指定了一组特定的签名，用于识别和处理智能合约交易。
// FiatRates: 指定获取法定货币汇率信息的服务或API。这允许Blockbook将加密货币的价值转换为法定货币的价值，便于用户理解和计算。
// FiatRatesParams: 获取法定货币汇率时使用的参数，可能包括API密钥、请求频率限制等。
// FiatRatesVsCurrencies: 法定货币汇率对比的货币列表，如USD、EUR等。这指定了需要获取哪些法定货币的汇率信息。
// BlockGolombFilterP: 与比特币BIP158(致密区块过滤器)中提到的Golomb编码过滤器相关的参数P。这个参数影响了过滤器的假阳性率，是轻客户端实现隐私增强型区块过滤的一个关键参数。
// BlockFilterScripts: 指定用于创建区块过滤器的脚本或地址列表。这些脚本用于过滤区块链数据，以减少客户端需要处理的数据量。
// BlockFilterUseZeroedKey: 一个布尔值，指定在创建区块过滤器时是否使用零化密钥。这可能是出于安全或隐私考虑，以确保过滤器的特定实现符合预期的隐私保护级别
type Config struct {
	CoinName                string `json:"coin_name"`
	CoinShortcut            string `json:"coin_shortcut"`
	CoinLabel               string `json:"coin_label"`
	FourByteSignatures      string `json:"fourByteSignatures"`
	FiatRates               string `json:"fiat_rates"`
	FiatRatesParams         string `json:"fiat_rates_params"`
	FiatRatesVsCurrencies   string `json:"fiat_rates_vs_currencies"`
	BlockGolombFilterP      uint8  `json:"block_golomb_filter_p"`
	BlockFilterScripts      string `json:"block_filter_scripts"`
	BlockFilterUseZeroedKey bool   `json:"block_filter_use_zeroed_key"`
}

// GetConfig loads and parses the config file and returns Config struct
func GetConfig(configFile string) (*Config, error) {
	if configFile == "" {
		return nil, errors.New("Missing blockchaincfg configuration parameter")
	}

	configFileContent, err := os.ReadFile(configFile)
	if err != nil {
		return nil, errors.Errorf("Error reading file %v, %v", configFile, err)
	}

	var cn Config
	err = json.Unmarshal(configFileContent, &cn)
	// 向现有错误追加信息
	if err != nil {
		return nil, errors.Annotatef(err, "Error parsing config file ")
	}
	return &cn, nil
}
