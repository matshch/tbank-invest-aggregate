# Maximum T-Bank Invest Account Value Evaluator
This tool tries its best to evaluate the maximum value of T-Bank Invest account
during a tax year. It is designed to get the best reasonable approximation for
the Report of Foreign Bank and Financial Accounts (FBAR), FinCEN Form 114.

## User guide
```shell
git clone https://github.com/matshch/tbank-invest-aggregate.git
cd tbank-invest-aggregate
cp config.yaml.example config.yaml
vim config.yaml
# Insert token from https://www.tbank.ru/invest/settings/api/
go run main.go
```

## Limitations
* Portfolio is estimated from its current value, and then operations are
  applied to get its state at the desired moment. It is not very exact method,
  please review the printed portfolio to see if it does make sense.
* There are some exceptions hardcoded in `main.go` to make up for operations
  log discrepancies. You may need to adapt these exceptions for your own case.
* You will need to update exchange rates hardcoded in the next tax year
  (or maybe pull updated version if I'll make one).
* You may need to update candle interval in source code to get better
  approximation, but note that T-Bank will rate limit you (which is handled
  automatically by their SDK).
