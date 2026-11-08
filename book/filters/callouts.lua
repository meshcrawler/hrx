-- Callouts, part dividers and listing includes for every output format.
--
--   ::: note / ::: tip / ::: warning      boxed sidebar with a label
--   ::: sidebar  (first heading = title)   longer aside
--   # Part I: Getting Started {.part}     book part divider
--   ```{.text include="generated/x.txt"}  listing read from a generated file

local labels = { note = "Note", tip = "Tip", warning = "Warning", sidebar = "" }

local function is_latex() return FORMAT:match("latex") ~= nil end

local function include_file(path)
  local f = io.open(path, "r")
  if not f then
    error("include: cannot read " .. path .. " (run book/tools/gen-listings.sh)")
  end
  local text = f:read("a")
  f:close()
  return (text:gsub("\n+$", ""))
end

function CodeBlock(el)
  local path = el.attributes.include
  if path then
    el.attributes.include = nil
    el.text = include_file(path)
    return el
  end
end

function Div(el)
  local kind
  for _, c in ipairs(el.classes) do
    if labels[c] ~= nil then kind = c end
  end
  if not kind then return nil end

  local title = labels[kind]
  local body = el.content
  if kind == "sidebar" and body[1] and body[1].t == "Header" then
    title = pandoc.utils.stringify(body[1].content)
    body = pandoc.List({ table.unpack(body, 2) })
  end

  if is_latex() then
    local out = pandoc.List({
      pandoc.RawBlock("latex", "\\begin{hrx" .. kind .. "}{" .. title .. "}"),
    })
    out:extend(body)
    out:insert(pandoc.RawBlock("latex", "\\end{hrx" .. kind .. "}"))
    return out
  end

  local out = pandoc.List({})
  if title ~= "" then
    out:insert(pandoc.Div(pandoc.Plain({ pandoc.Strong({ pandoc.Str(title) }) }),
      pandoc.Attr("", { "callout-title" })))
  end
  out:extend(body)
  return pandoc.Div(out, pandoc.Attr(el.identifier, { "callout", "callout-" .. kind }))
end

function Header(el)
  if el.level == 1 and el.classes:includes("part") and is_latex() then
    -- LaTeX prints "Part I" itself, so drop a leading "Part I: " from the title.
    local title = pandoc.utils.stringify(el.content):gsub("^Part [IVXLC]+:%s*", "")
    return pandoc.RawBlock("latex", "\\part{" .. title .. "}")
  end
end
