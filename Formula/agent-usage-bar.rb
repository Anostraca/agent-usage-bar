class AgentUsageBar < Formula
  desc "SwiftBar plugin showing Claude, Codex and Antigravity usage limits"
  homepage "https://github.com/Anostraca/agent-usage-bar"
  url "https://github.com/Anostraca/agent-usage-bar/archive/refs/tags/v0.3.1.tar.gz"
  sha256 "4ddbb2182d09550d55a8a86a07c594136890bc27c2416f0aa99ace2331aae28a"
  license "MIT"
  head "https://github.com/Anostraca/agent-usage-bar.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}")
  end

  def caveats
    <<~EOS
      Requires SwiftBar:
        brew install --cask swiftbar
      Then write the SwiftBar plugin:
        agent-usage-bar --install-plugin
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/agent-usage-bar --version")
  end
end
