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
)

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
			{Callback("📈 Полы", cbRefresh, "floor_list")},
			{Callback("📖 Лесенка", cbRefresh, "book_list")},
			{Callback("🔄 Сделки", cbRefresh, "hist_list")},
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
			"Следи за тем, что интересует:",
		Rows: [][]Button{
			{Callback("👁️ Мой список", cbRefresh, "watchlist")},
			{Callback("📌 Добавить", cbRefresh, "watch_add")},
			{Callback("🔕 Отключить звук", cbRefresh, "mute_menu")},
			{Callback("🔙 Назад", cbMenu, "")},
		},
	}
}

// settingsMenu returns settings/configuration options
func settingsMenu() Reply {
	return Reply{
		Text: "⚙️ <b>Настройки</b>\n\n" +
			"Настрой бота под себя:",
		Rows: [][]Button{
			{Callback("⚡ Автокупля", cbAuto, "")},
			{Callback("💰 Лимиты", cbRefresh, "limits")},
			{Callback("🔐 Авторизация", cbRefresh, "auth")},
			{Callback("🔙 Назад", cbMenu, "")},
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

// helpMenuReply returns help/reference information
func helpMenuReply() Reply {
	return Reply{
		Text: "📚 <b>Справка</b>\n\n" +
			"<b>📊 Рынок:</b>\n" +
			"<code>/gram</code> — курс GRAM и лаги\n" +
			"<code>/floor Коллекция [/Модель]</code> — полы и предложения\n" +
			"<code>/book Коллекция/Модель</code> — лесенка цен\n" +
			"<code>/hist Коллекция/Модель</code> — история сделок\n" +
			"<code>/val ID</code> — оценка лота\n\n" +
			"<b>💼 Портфель:</b>\n" +
			"<code>/pos</code> — твои позиции\n" +
			"<code>/portfolio</code> — рекомендации\n" +
			"<code>/advice ID</code> — как выходить\n" +
			"<code>/history ID</code> — история позиции\n" +
			"<code>/cost ID цена</code> — установить цену\n" +
			"<code>/exit ID цена</code> — выход\n" +
			"<code>/pnl</code> — твоя прибыль\n" +
			"<code>/balance</code> — баланс\n" +
			"<code>/relist ID</code> — переоценить\n\n" +
			"<b>⏰ Алерты:</b>\n" +
			"<code>/watch Коллекция/Модель [цена]</code> — добавить в список\n" +
			"<code>/unwatch Коллекция/Модель</code> — удалить\n" +
			"<code>/watchlist</code> — твой список\n" +
			"<code>/mute Коллекция [часы]</code> — отключить звук\n" +
			"<code>/unmute Коллекция</code> — включить обратно\n\n" +
			"<b>⚡ Автокупля:</b>\n" +
			"<code>/arm</code> — включить автопилот\n" +
			"<code>/disarm</code> — выключить\n" +
			"<code>/limits</code> — показать лимиты\n" +
			"<code>/limits set ключ значение</code> — <code>/limits set max_ticket 50</code>\n\n" +
			"<b>⚙️ Прочее:</b>\n" +
			"<code>/status</code> — состояние системы\n" +
			"<code>/auth authData</code> — обновить авторизацию\n\n" +
			"<i>Коллекция и модель разделены слешем: </i><code>/book Plush Pepe / Pink Diamond</code>",
		Rows: [][]Button{
			{Callback("🔙 Назад", cbMenu, "")},
		},
	}
}
