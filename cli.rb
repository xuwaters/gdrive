#!/usr/bin/env ruby

require "thor"

class CLI < Thor
  include Thor::Actions

  desc "build", "build gdrive"
  def build
    run <<~CMD.strip
      go build -o bin/gdrive gdrive/cmd/gdrive
    CMD
  end
end

CLI.start if caller.empty?
