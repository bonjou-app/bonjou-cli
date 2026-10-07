class Bonjou < Formula
  desc "Terminal-based LAN chat and transfer application"
  homepage "https://github.com/kodolabs-hq/bonjou-cli"
  version "1.2.0"

  on_macos do
    url "https://github.com/kodolabs-hq/bonjou-cli/releases/download/v1.2.0/bonjou-macos"
    sha256 "33ef78bf618d8ffcff9aa87e7da50e3f249f566c8a0619b09442b7f109b0f876"
  end

  def install
    bin.install "bonjou-macos" => "bonjou"
  end

  def caveats
    <<~EOS
      Bonjou expects UDP discovery on port 46320 and TCP messaging on port 46321.
      Ensure these ports are open on your firewall.
    EOS
  end

  test do
    pipe_output("#{bin}/bonjou", "@exit\n")
  end
end
