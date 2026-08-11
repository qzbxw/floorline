package bot

// Menu callback identifiers
const (
	cbMenu      = "m_menu"      // main menu
	cbMarket    = "m_market"    // market submenu
	cbPortfolio = "m_portfolio" // portfolio submenu
	cbAlerts    = "m_alerts"    // alerts/watch submenu
	cbSettings  = "m_settings"  // settings submenu
	cbAuto      = "m_auto"      // auto-buy settings
	cbHelp      = "m_help"      // help menu
	cbPick      = "m_pick"      // collection picker for a drill-down view
)

// back appends a navigation row to a Reply built by the backend.
//
// Backend views carry their own action buttons but know nothing about the menu
// they were opened from, so without this every data screen is a dead end that
// can only be escaped by typing /start again.
func back(r Reply, target, label string) Reply {
	return backWith(r, target, "", label)
}

// backWith is back for targets that need a payload, such as the collection
// picker, which must be told which view to return to.
func backWith(r Reply, target, data, label string) Reply {
	if r.Empty() {
		return r
	}
	return r.WithRow(Callback(label, target, data), Callback("🏠 Меню", cbMenu, ""))
}

// mainMenu returns the main dashboard with category buttons
func mainMenu() Reply {
	return Reply{
		Text: "⚙️ <b>Floorline</b> — Tonnel трейдинг\n\n" +
			"🎯 Давай торговать:",
		Rows: [][]Button{
			{Callback("📊 Рынок", cbMarket, "")},
			{Callback("💼 Портфель", cbPortfolio, "")},
			{Callback("⏰ Алерты", cbAlerts, "")},
			{Callback("⚙️ Настройки", cbSettings, "")},
			{Callback("📚 Справка", cbHelp, "")},
		},
	}
}

// marketMenu returns market analysis options
func marketMenu() Reply {
	return Reply{
		Text: "📊 <b>Анализ рынка</b>\n\n" +
			"Что хочешь посмотреть?",
		Rows: [][]Button{
			{Callback("💹 GRAM/USDT", cbRefresh, "gram")},
			{Callback("📈 Флор", cbPick, "floor"), Callback("📖 Стакан", cbPick, "book")},
			{Callback("🕒 Сделки", cbPick, "hist"), Callback("💰 Оценка лота", cbMarket, "val")},
			{Callback("🔙 Назад", cbMenu, "")},
		},
	}
}

// portfolioMenu returns portfolio options
func portfolioMenu() Reply {
	return Reply{
		Text: "💼 <b>Портфель</b>\n\n" +
			"Смотри свои позиции и прибыль:",
		Rows: [][]Button{
			{Callback("📍 Позиции", cbRefresh, "pos")},
			{Callback("📊 Обзор", cbRefresh, "portfolio")},
			{Callback("💰 P&L", cbRefresh, "pnl")},
			{Callback("💵 Баланс", cbRefresh, "balance")},
			{Callback("🔄 Статус", cbRefresh, "status")},
			{Callback("🔙 Назад", cbMenu, "")},
		},
	}
}

// alertsMenu returns alert/watchlist options
func alertsMenu() Reply {
	return Reply{
		Text: "⏰ <b>Алерты</b>\n\n" +
			"Следи за интересным:",
		Rows: [][]Button{
			{Callback("👁️ Мой список", cbRefresh, "watchlist")},
			{Callback("📌 Подписаться", cbAlerts, "watch")},
			{Callback("🔕 Отключить звук", cbAlerts, "mute")},
			{Callback("🔙 Назад", cbMenu, "")},
		},
	}
}

// alertsActionMenu returns instruction for alerts
func alertsActionMenu(action string) Reply {
	texts := map[string]string{
		"watch": "📌 <b>Подписаться на коллекцию</b>\n\n" +
			"Введи: <code>/watch Коллекция / Модель [цена]</code>\n\n" +
			"Примеры:\n" +
			"<code>/watch Plush Pepe / Pink Diamond</code>\n" +
			"<code>/watch Plush Pepe / Pink Diamond 50</code> — до цены 50 USDT",
		"mute": "🔕 <b>Отключить звук</b>\n\n" +
			"Введи: <code>/mute Коллекция [/ Модель] [часы]</code>\n\n" +
			"Примеры:\n" +
			"<code>/mute Plush</code> — 1 час\n" +
			"<code>/mute Plush Pepe / Pink Diamond 4</code> — 4 часа",
	}
	text := texts[action]
	if text == "" {
		text = "Введи команду"
	}
	return Reply{
		Text: text,
		Rows: [][]Button{
			{Callback("🔙 Назад", cbAlerts, "")},
		},
	}
}

// settingsMenu returns settings/configuration options
func settingsMenu() Reply {
	return Reply{
		Text: "⚙️ <b>Настройки</b>\n\n" +
			"Настрой под себя:",
		Rows: [][]Button{
			{Callback("⚡ Автокупля", cbAuto, "")},
			{Callback("💰 Лимиты", cbRefresh, "limits")},
			{Callback("🔐 Сессия", cbSettings, "auth")},
			{Callback("🔙 Назад", cbMenu, "")},
		},
	}
}

// settingsActionMenu returns instruction for settings
func settingsActionMenu(action string) Reply {
	texts := map[string]string{
		"auth": "🔐 <b>Обновить сессию Tonnel</b>\n\n" +
			"Сессия протухла? Берёшь новую initData из Tonnel мини-апп:\n\n" +
			"DevTools → Console → выполни:\n" +
			"<code>copy(Telegram.WebApp.initData)</code>\n\n" +
			"Потом отправь:\n" +
			"<code>/auth &lt;authData&gt;</code>\n\n" +
			"<i>Сообщение удалится автоматом (в истории чата не останется).</i>",
	}
	text := texts[action]
	if text == "" {
		text = "Введи команду"
	}
	return Reply{
		Text: text,
		Rows: [][]Button{
			{Callback("🔙 Назад", cbSettings, "")},
		},
	}
}

// autoMenu returns auto-buy settings
func autoMenu() Reply {
	return Reply{
		Text: "⚡ <b>Автокупля</b>\n\n" +
			"Включи режим автопилота:",
		Rows: [][]Button{
			{Callback("✅ Включить", cbRefresh, "arm")},
			{Callback("❌ Выключить", cbRefresh, "disarm")},
			{Callback("📋 Лимиты", cbRefresh, "limits")},
			{Callback("🔙 Назад", cbSettings, "")},
		},
	}
}

// marketActionMenu covers the one market view that needs a number typed in:
// a gift id cannot be offered as a button because it is not in any list.
func marketActionMenu(action string) Reply {
	if action != "val" {
		return marketMenu()
	}
	return Reply{
		Text: "💰 <b>Оценка лота</b>\n\n" +
			"Пришли <code>/val ID</code> — ID видно в ссылке лота на Tonnel.\n\n" +
			"Например: <code>/val 10368454</code>",
		Rows: [][]Button{
			{Callback("🔙 Назад", cbMarket, ""), Callback("🏠 Меню", cbMenu, "")},
		},
	}
}

// helpMenuReply returns help/reference information
func helpMenuReply() Reply {
	return Reply{
		Text: "📚 <b>Справка</b>\n\n" +
			"<b>📊 Рынок:</b>\n" +
			"<code>/gram</code> — курс GRAM и лаги\n" +
			"<code>/floor Коллекция [/Модель]</code> — полы и предложения\n" +
			"<code>/book Коллекция/Модель</code> — лесенка\n" +
			"<code>/hist Коллекция/Модель</code> — сделки\n" +
			"<code>/val ID</code> — оценка лота\n\n" +
			"<b>💼 Портфель:</b>\n" +
			"<code>/pos</code> — позиции\n" +
			"<code>/portfolio</code> — советы\n" +
			"<code>/advice ID</code> — как выходить\n" +
			"<code>/history ID</code> — история\n" +
			"<code>/cost ID цена</code> — цена покупки\n" +
			"<code>/exit ID цена</code> — выход\n" +
			"<code>/pnl</code> — профит/убыток\n" +
			"<code>/balance</code> — баланс\n" +
			"<code>/relist ID</code> — переоценить\n\n" +
			"<b>⏰ Алерты:</b>\n" +
			"<code>/watch Коллекция/Модель [цена]</code> — подписаться\n" +
			"<code>/unwatch Коллекция/Модель</code> — отписаться\n" +
			"<code>/watchlist</code> — подписки\n" +
			"<code>/mute Коллекция [часы]</code> — тишина\n" +
			"<code>/unmute Коллекция</code> — звук обратно\n\n" +
			"<b>⚡ Автокупля:</b>\n" +
			"<code>/arm</code> — включить\n" +
			"<code>/disarm</code> — выключить\n" +
			"<code>/limits</code> — лимиты\n" +
			"<code>/limits set ключ значение</code> — <code>/limits set max_ticket 50</code>\n\n" +
			"<b>⚙️ Система:</b>\n" +
			"<code>/status</code> — состояние\n" +
			"<code>/auth authData</code> — сессия\n\n" +
			"<i>Коллекция и модель разделены слешем: </i><code>/book Plush Pepe / Pink Diamond</code>",
		Rows: [][]Button{
			{Callback("🔙 Назад", cbMenu, "")},
		},
	}
}
