# 来源与第三方许可

本项目是面向 [TokenFlux/TokenRouter](https://github.com/TokenFlux/TokenRouter) 的第三方独立插件，不是官方发行版。

插件以 LGPL-3.0-or-later 发布，许可证见 `LICENSE`；该许可证引用的 GNU GPL v3 完整文本见 `COPYING`。

账户余额解析、金额模型、统计及加密等部分代码沿用此前基于 TokenRouter 的定制实现。New API 账户余额行为对照 TokenRouter v0.7.1 的 `backend/internal/upstream/usageprovider/adapters.go` 实现，插件只采用账户余额，并使用十进制与精确整数校验。

`vendor/github.com/TokenFlux/TokenRouter/plugin-sdk/go/` 包含官方插件 SDK，使用 MIT 许可证，保留原始版权声明与许可证。其他第三方依赖及其版本见 `go.mod`、`go.sum` 和 `vendor/modules.txt`；各依赖许可证保留在相应 `vendor` 目录中。

当前 SDK 使用随源码提供的 vendor 副本构建。`go.mod` 的本地 replace 用于原开发环境；本仓库的构建和测试命令显式使用 `-mod=vendor`，不需要相邻的 TokenRouter 源码目录。更新依赖前请先准备兼容的 SDK 源码，不要直接移除 vendor 后执行依赖更新。
