package tui

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
)

// ModelLister is the optional live /v1/models seam used by /models and /model.
type ModelLister interface {
	Models(ctx context.Context) ([]llm.ModelInfo, error)
}

// ModelOption describes a selectable model row. Source is "live" or an alias
// label such as "alias dsv4"; Provider is informational for display.
type ModelOption struct {
	ID            string
	ContextLength int
	Provider      string
	Source        string
	Alias         string
}

// RuntimeConfig is the TUI's current model runtime state. It is copied into the
// runner for each submitted task, so switches apply to the next run only.
type RuntimeConfig struct {
	Provider      string
	Endpoint      string
	APIKey        string
	Model         string
	ContextTokens int
	Temperature   float64
	ToolFormat    string
	NoThink       bool
	NoThinkLocked bool
	NewClient     func(endpoint, model, apiKey string) llm.LLMClient
	UseNewClient  bool
}

type modelPicker struct {
	all    []modelPickerItem
	filter string
	list   list.Model
}

type modelPickerItem struct {
	ModelOption
}

func (i modelPickerItem) FilterValue() string {
	return strings.TrimSpace(i.ID + " " + i.Provider + " " + i.Source)
}

func (i modelPickerItem) Title() string {
	return i.ID
}

func (i modelPickerItem) Description() string {
	fields := []string{formatContext(i.ContextLength)}
	if i.Provider != "" {
		fields = append(fields, "provider "+i.Provider)
	}
	if i.Source != "" {
		fields = append(fields, i.Source)
	}
	return strings.Join(fields, " · ")
}

type modelPickerDelegate struct{}

func (modelPickerDelegate) Height() int  { return 2 }
func (modelPickerDelegate) Spacing() int { return 0 }
func (modelPickerDelegate) Update(tea.Msg, *list.Model) tea.Cmd {
	return nil
}

func (modelPickerDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(modelPickerItem)
	if !ok {
		return
	}
	marker := "  "
	if index == m.Index() {
		marker = "> "
	}
	title := marker + it.Title()
	desc := "  " + it.Description()
	if index == m.Index() {
		title = accent.Render(title)
		desc = accent.Render(desc)
	} else {
		desc = muted.Render(desc)
	}
	fmt.Fprintf(w, "%s\n%s", title, desc) //nolint:errcheck
}

func newModelPicker(options []ModelOption, width int) *modelPicker {
	items := make([]modelPickerItem, 0, len(options))
	for _, opt := range options {
		if strings.TrimSpace(opt.ID) == "" {
			continue
		}
		items = append(items, modelPickerItem{ModelOption: opt})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Provider == items[j].Provider {
			if items[i].Source == items[j].Source {
				return items[i].ID < items[j].ID
			}
			return items[i].Source < items[j].Source
		}
		return items[i].Provider < items[j].Provider
	})

	p := &modelPicker{all: items}
	p.list = list.New(modelPickerListItems(items), modelPickerDelegate{}, max(width-6, 20), 6)
	p.list.SetShowTitle(false)
	p.list.SetShowFilter(false)
	p.list.SetShowStatusBar(false)
	p.list.SetShowPagination(false)
	p.list.SetShowHelp(false)
	p.list.SetFilteringEnabled(false)
	return p
}

func modelPickerListItems(items []modelPickerItem) []list.Item {
	out := make([]list.Item, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func (p *modelPicker) setSize(width int) {
	p.list.SetWidth(max(width-6, 20))
	p.list.SetHeight(6)
}

func (p *modelPicker) applyFilter() {
	filter := strings.ToLower(strings.TrimSpace(p.filter))
	filtered := make([]modelPickerItem, 0, len(p.all))
	for _, item := range p.all {
		if filter == "" || strings.Contains(strings.ToLower(item.FilterValue()), filter) {
			filtered = append(filtered, item)
		}
	}
	_ = p.list.SetItems(modelPickerListItems(filtered))
	p.list.Select(0)
}

func (m Model) slashModels() Model {
	live, err := m.fetchLiveModels()
	if err != nil {
		m = m.appendItem(infoItem{text: "live models unavailable: " + err.Error()})
	}
	if len(live) == 0 && len(m.modelOptions) == 0 {
		return m.appendItem(infoItem{text: "no models available"})
	}
	m = m.appendItem(infoItem{text: "models:"})
	for _, opt := range live {
		m = m.appendItem(infoItem{text: "  " + opt.ID + " · " + formatContext(opt.ContextLength) + " · live"})
	}
	for _, opt := range m.modelOptions {
		m = m.appendItem(infoItem{text: "  " + opt.ID + " · " + formatContext(opt.ContextLength) + " · " + opt.Provider + " · " + opt.Source})
	}
	return m
}

func (m Model) openModelPicker() Model {
	live, err := m.fetchLiveModels()
	if err != nil {
		m = m.appendItem(infoItem{text: "live models unavailable: " + err.Error()})
	}
	options := append([]ModelOption{}, live...)
	options = append(options, m.modelOptions...)
	if len(options) == 0 {
		return m.appendItem(infoItem{text: "no models available"})
	}
	m.picker = newModelPicker(options, m.width)
	return m.appendItem(infoItem{text: "opening model picker..."})
}

func (m Model) fetchLiveModels() ([]ModelOption, error) {
	lister := m.currentModelLister()
	if lister == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models, err := lister.Models(ctx)
	if err != nil {
		return nil, err
	}
	options := make([]ModelOption, 0, len(models))
	for _, model := range models {
		options = append(options, ModelOption{
			ID:            model.ID,
			ContextLength: model.ContextLength,
			Provider:      "current",
			Source:        "live",
		})
	}
	return options, nil
}

func (m Model) currentModelLister() ModelLister {
	if m.runtime.UseNewClient && m.runtime.NewClient != nil {
		if client := m.runtime.NewClient(m.runtime.Endpoint, m.runtime.Model, m.runtime.APIKey); client != nil {
			if lister, ok := client.(ModelLister); ok {
				return lister
			}
		}
	}
	return m.modelLister
}

func (m Model) handleModelPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.picker == nil {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		m.picker = nil
		return m.appendItem(infoItem{text: "model picker cancelled"}), nil
	case tea.KeyEnter:
		item, ok := m.picker.list.SelectedItem().(modelPickerItem)
		if !ok {
			return m, nil
		}
		m.picker = nil
		return m.selectModelOption(item.ModelOption), nil
	case tea.KeyBackspace:
		if m.picker.filter != "" {
			runes := []rune(m.picker.filter)
			m.picker.filter = string(runes[:len(runes)-1])
			m.picker.applyFilter()
		}
		return m, nil
	case tea.KeyRunes:
		m.picker.filter += string(msg.Runes)
		m.picker.applyFilter()
		return m, nil
	}

	var cmd tea.Cmd
	m.picker.list, cmd = m.picker.list.Update(msg)
	return m, cmd
}

func (m Model) selectModelName(name string) Model {
	name = strings.TrimSpace(name)
	for _, opt := range m.modelOptions {
		if opt.Alias == name {
			return m.selectModelOption(opt)
		}
	}
	if cfg, ok := config.ResolveModelAlias(name, m.profilePath, m.getenv); ok {
		return m.applyResolvedModel(cfg, name, false)
	}
	live, err := m.fetchLiveModels()
	if err != nil {
		m = m.appendItem(infoItem{text: "live model validation unavailable: " + err.Error()})
	}
	for _, opt := range live {
		if opt.ID == name {
			if opt.ContextLength > 0 {
				m.runtime.ContextTokens = opt.ContextLength
			}
			return m.applyRawModel(name, "model: "+name)
		}
	}
	if m.currentModelLister() != nil {
		m = m.appendItem(infoItem{text: "warning: model " + name + " not found in live model list; switching anyway"})
	}
	return m.applyRawModel(name, "model: "+name)
}

func (m Model) selectModelOption(opt ModelOption) Model {
	if opt.Alias != "" {
		if opt.Provider != "" {
			if cfg, ok := config.ResolveProviderModelAlias(opt.Provider, opt.Alias, m.profilePath, m.getenv); ok {
				return m.applyResolvedModel(cfg, opt.Alias, true)
			}
		}
		if cfg, ok := config.ResolveModelAlias(opt.Alias, m.profilePath, m.getenv); ok {
			return m.applyResolvedModel(cfg, opt.Alias, true)
		}
	}
	if opt.ContextLength > 0 {
		m.runtime.ContextTokens = opt.ContextLength
	}
	return m.applyRawModel(opt.ID, "model: "+opt.ID)
}

func (m Model) applyResolvedModel(cfg config.Config, alias string, fromPicker bool) Model {
	m.runtime.Provider = cfg.Provider
	m.runtime.Endpoint = cfg.Endpoint
	m.runtime.APIKey = cfg.APIKey
	m.runtime.Model = cfg.Model
	m.runtime.ContextTokens = cfg.MaxContextTokens
	m.runtime.Temperature = cfg.Temperature
	m.runtime.ToolFormat = cfg.ToolFormat
	if !m.runtime.NoThinkLocked {
		m.runtime.NoThink = cfg.NoThink
	}
	m.modelName = cfg.Model
	m.status.model = cfg.Model
	m.status.provider = cfg.Provider
	msg := "model: " + cfg.Provider + "/" + cfg.Model
	if alias != "" && alias != cfg.Model {
		msg += " (alias " + alias + ")"
	}
	if fromPicker {
		msg = "selected " + msg
	}
	return m.appendItem(infoItem{text: msg})
}

func (m Model) applyRawModel(name, msg string) Model {
	m.runtime.Model = name
	m.runtime.Provider = ""
	m.modelName = name
	m.status.model = name
	m.status.provider = ""
	return m.appendItem(infoItem{text: msg})
}

func (m Model) renderModelPicker() string {
	if m.picker == nil {
		return ""
	}
	m.picker.setSize(m.width)
	filter := m.picker.filter
	if filter == "" {
		filter = "type to filter"
	} else {
		filter = "filter: " + filter
	}
	title := "Select model for next run"
	gap := max(m.width-6-lipgloss.Width(title)-lipgloss.Width(filter), 1)
	header := title + strings.Repeat(" ", gap) + accent.Render(filter)
	body := m.picker.list.View()
	footer := muted.Render("↑/↓ move  Enter select  Esc cancel")
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(accentColor).
		Width(max(m.width-2, 20)).
		Render(strings.Join([]string{header, body, footer}, "\n"))
}

func formatContext(n int) string {
	if n <= 0 {
		return "ctx unknown"
	}
	if n%1000 == 0 {
		return fmt.Sprintf("%dk ctx", n/1000)
	}
	return fmt.Sprintf("%d ctx", n)
}
